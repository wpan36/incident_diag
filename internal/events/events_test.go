package events

import (
	"testing"
	"time"
)

func TestStreamKeyNamesTheRun(t *testing.T) {
	// The name is the contract M26 reads by, and one stream per run is what
	// keeps a busy run from evicting a quiet one's history.
	const runID = "01K5Z0000000000000000000AB"
	if got, want := StreamKey(runID), "run:"+runID+":events"; got != want {
		t.Errorf("StreamKey(%q) = %q, want %q", runID, got, want)
	}
}

func TestTheThreeEventNames(t *testing.T) {
	// Asserted literally because they are a wire contract with the browser:
	// renaming one is a breaking change, and it should fail here rather than
	// in a timeline that silently stops updating.
	for name, want := range map[string]string{
		RunStarted:    "run.started",
		StepCompleted: "step.completed",
		RunFinished:   "run.finished",
	} {
		if name != want {
			t.Errorf("event name = %q, want %q", name, want)
		}
	}
}

func TestTheFakeRecordsWhatItWasAskedToPublish(t *testing.T) {
	var f FakePublisher
	if err := f.Publish(t.Context(), "run-1", Event{Name: RunStarted, Payload: 1}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := f.Trim(t.Context(), "run-1"); err != nil {
		t.Fatalf("Trim: %v", err)
	}

	if got := f.Names(); len(got) != 1 || got[0] != RunStarted {
		t.Errorf("Names() = %v, want [%s]", got, RunStarted)
	}
	if got := f.Trims(); len(got) != 1 || got[0] != "run-1" {
		t.Errorf("Trims() = %v, want [run-1]", got)
	}

	// Err is how a test says "Redis is down", and it has to stop both calls:
	// the worker must survive a publish failure and a trim failure alike.
	f.Reset()
	f.Err = errTest
	if err := f.Publish(t.Context(), "run-1", Event{Name: RunFinished}); err != errTest {
		t.Errorf("Publish with Err set returned %v, want the error", err)
	}
	if err := f.Trim(t.Context(), "run-1"); err != errTest {
		t.Errorf("Trim with Err set returned %v, want the error", err)
	}
	if len(f.Events()) != 0 || len(f.Trims()) != 0 {
		t.Error("a failing fake recorded something")
	}
}

type testError struct{}

func (testError) Error() string { return "redis is down" }

var errTest = testError{}

func TestTheFakeReaderResumesAndBlocks(t *testing.T) {
	f := &FakeReader{Block: 5 * time.Millisecond}
	f.Add(ReadEvent{ID: "1-0", Name: RunStarted}, ReadEvent{ID: "2-0", Name: RunFinished})

	names := func(after string, call func(func(ReadEvent) error) error) []string {
		t.Helper()
		var got []string
		if err := call(func(ev ReadEvent) error {
			got = append(got, ev.Name)
			return nil
		}); err != nil {
			t.Fatalf("reading: %v", err)
		}
		return got
	}

	got := names("", func(fn func(ReadEvent) error) error { return f.Replay(t.Context(), "run-1", "", fn) })
	if len(got) != 2 {
		t.Errorf("Replay from the start yielded %v, want both events", got)
	}
	got = names("1-0", func(fn func(ReadEvent) error) error { return f.Replay(t.Context(), "run-1", "1-0", fn) })
	if len(got) != 1 || got[0] != RunFinished {
		t.Errorf("Replay after 1-0 yielded %v, want just run.finished", got)
	}

	// An empty follow waits out its block, which is what makes the SSE
	// handler's idle round reachable without a Redis.
	start := time.Now()
	got = names("2-0", func(fn func(ReadEvent) error) error { return f.Follow(t.Context(), "run-1", "2-0", fn) })
	if len(got) != 0 {
		t.Errorf("Follow past the end yielded %v", got)
	}
	if time.Since(start) < 5*time.Millisecond {
		t.Error("Follow returned without waiting out its block")
	}

	f.Err = errTest
	if err := f.Replay(t.Context(), "run-1", "", func(ReadEvent) error { return nil }); err != errTest {
		t.Errorf("Replay with Err set returned %v, want the error", err)
	}
	if err := f.Follow(t.Context(), "run-1", "", func(ReadEvent) error { return nil }); err != errTest {
		t.Errorf("Follow with Err set returned %v, want the error", err)
	}
}
