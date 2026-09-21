package events

import (
	"context"
	"sync"
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
