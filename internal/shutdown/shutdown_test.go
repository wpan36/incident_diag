package shutdown

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestCloseRunsInReverseOrder(t *testing.T) {
	var (
		mu    sync.Mutex
		order []string
	)
	note := func(name string) func(context.Context) error {
		return func(context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
			return nil
		}
	}

	var g Group
	g.Add("database", note("database"))
	g.Add("kafka", note("kafka"))
	g.Add("http server", note("http server"))

	if err := g.Close(time.Second); err != nil {
		t.Fatalf("Close(): %v", err)
	}

	want := []string{"http server", "kafka", "database"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("close order = %v, want %v", order, want)
	}
}

func TestCloseReportsEveryFailure(t *testing.T) {
	first := errors.New("kafka is unhappy")
	second := errors.New("database is unhappy")

	var g Group
	g.Add("database", func(context.Context) error { return second })
	g.Add("kafka", func(context.Context) error { return first })

	err := g.Close(time.Second)
	if err == nil {
		t.Fatal("Close() = nil, want two errors")
	}
	// Joined, not first-wins: a failure during shutdown is often the only clue
	// about what went wrong, and hiding one behind another wastes it.
	if !errors.Is(err, first) || !errors.Is(err, second) {
		t.Errorf("Close() = %v, want both underlying errors", err)
	}
	if !strings.Contains(err.Error(), "closing kafka") || !strings.Contains(err.Error(), "closing database") {
		t.Errorf("Close() = %v, want both resource names", err)
	}
}

func TestCloseAbandonsAHangingCloser(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	var (
		mu      sync.Mutex
		started []string
	)
	start := func(name string) {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, name)
	}

	var g Group
	// Registered first, so it closes last.
	g.Add("database", func(context.Context) error {
		start("database")
		return nil
	})
	g.Add("stubborn", func(ctx context.Context) error {
		start("stubborn")
		<-release // deliberately ignores ctx
		return nil
	})

	began := time.Now()
	err := g.Close(50 * time.Millisecond)
	elapsed := time.Since(began)

	if err == nil {
		t.Fatal("Close() = nil, want a deadline error for the stubborn closer")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Close() = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "closing stubborn") {
		t.Errorf("Close() = %v, want the stubborn resource named", err)
	}
	// The guarantee that matters: Close returns near the deadline instead of
	// waiting for a closer that will never finish.
	if elapsed > time.Second {
		t.Errorf("Close took %v; it should abandon the hanging closer", elapsed)
	}

	// The deadline is shared, so the database closer is still invoked on a
	// best-effort basis but is reported as incomplete. Give the abandoned
	// goroutines a moment to be scheduled before checking.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(started) != 2 {
		t.Errorf("closers invoked = %v, want both to have been attempted", started)
	}
	if !strings.Contains(err.Error(), "closing database") {
		t.Errorf("Close() = %v, want the database closer reported as incomplete "+
			"once the shared deadline had passed", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	calls := 0
	var g Group
	g.Add("thing", func(context.Context) error {
		calls++
		return nil
	})

	if err := g.Close(time.Second); err != nil {
		t.Fatalf("first Close(): %v", err)
	}
	if err := g.Close(time.Second); err != nil {
		t.Fatalf("second Close(): %v", err)
	}
	// A deferred Close plus an explicit one on the error path is a normal
	// shape in main; closing twice must not close resources twice.
	if calls != 1 {
		t.Errorf("closer ran %d times, want 1", calls)
	}
}

func TestEmptyGroupCloses(t *testing.T) {
	var g Group
	if err := g.Close(time.Second); err != nil {
		t.Errorf("Close() on an empty group = %v", err)
	}
}

func TestAddAfterClosePanics(t *testing.T) {
	var g Group
	if err := g.Close(time.Second); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("Add after Close did not panic")
		}
	}()
	g.Add("late", func(context.Context) error { return nil })
}

func TestClosersReceiveTheDeadline(t *testing.T) {
	var g Group
	g.Add("checks", func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("close function received a context with no deadline")
			return nil
		}
		if time.Until(deadline) > time.Second {
			t.Errorf("deadline is %v away, want about 200ms", time.Until(deadline))
		}
		return nil
	})
	if err := g.Close(200 * time.Millisecond); err != nil {
		t.Fatalf("Close(): %v", err)
	}
}

func TestContextCancelsOnSignal(t *testing.T) {
	ctx, stop := Context(context.Background())
	defer stop()

	select {
	case <-ctx.Done():
		t.Fatal("context was cancelled before any signal")
	default:
	}

	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("sending SIGTERM: %v", err)
	}

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context was not cancelled after SIGTERM")
	}
}
