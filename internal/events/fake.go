package events

import (
	"context"
	"sync"
	"time"
)

// FakePublisher records what would have been published.
//
// It ships in the package rather than in a _test.go file for the reason
// mq.FakeProducer does: the code that most needs testing without Redis —
// internal/agentrun — lives somewhere else.
//
// It is safe for concurrent use.
type FakePublisher struct {
	mu        sync.Mutex
	published []Published
	trims     []string

	// Err, when set, is returned by every Publish and every Trim and nothing
	// is recorded. It is how a test says "Redis is down".
	Err error
}

// Published is one event a FakePublisher was asked to send.
type Published struct {
	RunID string
	Event Event
}

// Publish implements Publisher.
func (f *FakePublisher) Publish(_ context.Context, runID string, e Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.published = append(f.published, Published{RunID: runID, Event: e})
	return nil
}

// Trim implements Publisher.
func (f *FakePublisher) Trim(_ context.Context, runID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.trims = append(f.trims, runID)
	return nil
}

// Events returns a copy of what has been published so far.
func (f *FakePublisher) Events() []Published {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Published(nil), f.published...)
}

// Names returns just the event names published, in order, which is what most
// assertions are about.
func (f *FakePublisher) Names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.published))
	for _, p := range f.published {
		out = append(out, p.Event.Name)
	}
	return out
}

// Trims returns the run ids whose streams were emptied.
func (f *FakePublisher) Trims() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.trims...)
}

// Reset forgets everything recorded.
func (f *FakePublisher) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published, f.trims = nil, nil
}

// FakeReader is a Reader backed by a slice, so internal/api's tests need no
// Redis and still run in milliseconds.
//
// It ships beside FakePublisher and for the same reason: the code that most
// needs testing without Redis lives in another package.
//
// It is safe for concurrent use, because a test appends to it from one
// goroutine while the handler under test reads from another — and because a
// test that says "Redis failed mid-stream" has to say it while the handler is
// already blocked in Follow. The error and the block are therefore set through
// Fail and SetBlock rather than written as exported fields, which is what
// makes that test race-free.
type FakeReader struct {
	mu     sync.Mutex
	events []ReadEvent
	err    error
	block  time.Duration
}

// Add appends an event, as a publisher would.
func (f *FakeReader) Add(evs ...ReadEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, evs...)
}

// Fail makes every later Replay and Follow return err. It is how a test says
// "Redis failed", and it is safe to call while a handler is reading.
func (f *FakeReader) Fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// SetBlock sets how long Follow waits before reporting that nothing arrived.
// It stands in for SSE_READ_BLOCK, and a test sets it to a few milliseconds so
// an idle round is not an idle second.
func (f *FakeReader) SetBlock(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.block = d
}

// state reads the two knobs under the lock.
func (f *FakeReader) state() (time.Duration, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.block, f.err
}

// Replay implements Reader.
func (f *FakeReader) Replay(_ context.Context, _, after string, fn func(ReadEvent) error) (string, error) {
	if _, err := f.state(); err != nil {
		return "", err
	}
	return deliverFake(f.since(after), fn)
}

// Follow implements Reader.
//
// It returns immediately when something is already waiting, and otherwise
// sleeps out its block — which is what XREAD BLOCK does, and what makes the
// handler's "an event is its own keepalive" branch reachable in a test.
func (f *FakeReader) Follow(ctx context.Context, _, after string, fn func(ReadEvent) error) (string, error) {
	block, err := f.state()
	if err != nil {
		return "", err
	}
	pending := f.since(after)
	if len(pending) == 0 {
		select {
		case <-ctx.Done():
			return "", nil
		case <-time.After(block):
		}
		// Re-read: a test that says "Redis failed mid-stream" calls Fail
		// while this call is blocked, which is the only moment the handler's
		// error path can be reached from outside.
		if _, err := f.state(); err != nil {
			return "", err
		}
		pending = f.since(after)
	}
	return deliverFake(pending, fn)
}

// since returns the events after `after`, comparing ids as strings — which is
// correct for the fixed-width ids these tests use and is not what Redis does.
func (f *FakeReader) since(after string) []ReadEvent {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]ReadEvent, 0, len(f.events))
	for _, e := range f.events {
		if after == "" || e.ID > after {
			out = append(out, e)
		}
	}
	return out
}

// deliverFake mirrors Client.deliver, skip rule included: an event with no
// name or no data is read and not yielded, and it still advances the returned
// id. A fake that yielded it anyway would hide the spin that rule caused.
func deliverFake(evs []ReadEvent, fn func(ReadEvent) error) (string, error) {
	var last string
	for _, e := range evs {
		last = e.ID
		if e.Name == "" || len(e.Data) == 0 {
			continue
		}
		if err := fn(e); err != nil {
			return last, err
		}
	}
	return last, nil
}
