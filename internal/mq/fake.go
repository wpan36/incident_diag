package mq

import (
	"context"
	"sync"
)

// Produced is one message a FakeProducer was asked to send.
type Produced struct {
	Topic string
	Key   string
	Msg   any
}

// FakeProducer records what would have been produced.
//
// It ships in the package rather than in a _test.go file because the code that
// most needs testing without a broker — the reconciler — lives somewhere else.
// The alternative, an mqtest package, would be one more package for one type.
//
// It is safe for concurrent use, since the reconciler produces from its own
// goroutine.
type FakeProducer struct {
	mu   sync.Mutex
	msgs []Produced

	// Err, when set, is returned by every Produce and nothing is recorded. It
	// is how a test says "the broker is down".
	Err error
}

// Produce implements Producer.
func (f *FakeProducer) Produce(_ context.Context, topic, key string, msg any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.msgs = append(f.msgs, Produced{Topic: topic, Key: key, Msg: msg})
	return nil
}

// Messages returns a copy of what has been produced so far.
func (f *FakeProducer) Messages() []Produced {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Produced(nil), f.msgs...)
}

// Keys returns just the keys produced, which is what most assertions are about.
func (f *FakeProducer) Keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.msgs))
	for _, m := range f.msgs {
		out = append(out, m.Key)
	}
	return out
}

// Reset forgets everything recorded.
func (f *FakeProducer) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = nil
}
