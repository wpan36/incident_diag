// Package shutdown turns a termination signal into a cancelled context and
// closes resources in a defined order.
//
// Every binary here is a container that will eventually be sent SIGTERM, and
// every one of them holds things that need closing: an HTTP server, a Kafka
// client, database handles. Doing that by hand in each main function is how
// half of them end up leaking a connection or hanging past the runtime's grace
// period.
package shutdown

import (
	"context"
	"errors"
	"fmt"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Context returns a context cancelled when the process receives SIGINT or
// SIGTERM, together with a stop function that restores default signal handling.
//
// The stop function must be called, normally by defer in main. Until it is, the
// process ignores the default behaviour for those signals, which means a second
// Ctrl-C during a slow shutdown would not kill it.
func Context(parent context.Context) (context.Context, func()) {
	return signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
}

// closer is one registered resource.
type closer struct {
	name string
	fn   func(context.Context) error
}

// Group closes resources in the reverse of the order they were registered.
//
// Reverse order is what callers already expect from defer, and it is usually
// what correctness requires: an HTTP server registered after the database must
// stop accepting requests before the database it depends on goes away.
//
// A Group is safe for concurrent use, though registration normally happens on
// one goroutine during startup.
type Group struct {
	mu      sync.Mutex
	closers []closer
	closed  bool
}

// Add registers fn under name. The name appears in error messages, so it should
// say what the resource is ("http server", "kafka client").
//
// Adding to a Group that has already been closed is a programming error and
// panics, because the alternative is a resource that silently never gets
// closed.
func (g *Group) Add(name string, fn func(context.Context) error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		panic("shutdown: Add called after Close: " + name)
	}
	g.closers = append(g.closers, closer{name: name, fn: fn})
}

// Close runs every registered close function in reverse registration order,
// giving the whole sequence at most timeout to finish.
//
// The deadline is shared, not per closer: the caller cares that the process
// exits within its grace period, not how that budget is divided. Two
// consequences follow, and both are deliberate.
//
// A close function that ignores its context and blocks anyway cannot be
// stopped, so Close abandons it and returns rather than hanging with it. The
// goroutine it leaves behind keeps running until the process exits moments
// later, which means a close function must not write to anything whose
// lifetime ends when Close returns.
//
// A closer that hangs can consume the whole budget. The remaining closers are
// still invoked, best effort, but Close reports them as incomplete because it
// did not wait to see them finish. That is the honest answer: it does not know
// whether they succeeded.
//
// Every failure is reported. Close returns all of them joined, so one noisy
// resource does not hide another.
func (g *Group) Close(timeout time.Duration) error {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return nil
	}
	g.closed = true
	closers := g.closers
	g.mu.Unlock()

	// A fresh context: the context that triggered shutdown is already
	// cancelled, and passing that in would make every close function fail
	// immediately.
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var errs []error
	for i := len(closers) - 1; i >= 0; i-- {
		c := closers[i]
		if err := runClose(ctx, c); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runClose runs one close function, returning once it finishes or once ctx
// expires, whichever comes first.
func runClose(ctx context.Context, c closer) error {
	done := make(chan error, 1) // buffered so an abandoned goroutine can still exit
	go func() { done <- c.fn(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("closing %s: %w", c.name, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("closing %s: %w", c.name, ctx.Err())
	}
}
