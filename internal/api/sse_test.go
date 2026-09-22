package api

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/events"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/store"
)

// testRunID is a syntactically valid identifier, which is all validID checks.
const testRunID = "01K5Z0000000000000000000AB"

// fakeRuns stands in for the store's GetRun.
//
// Statuses are consumed one per call and the last one repeats, which is how a
// test says "RUNNING, then terminal" — the case where run.finished was never
// published and the stream has to end on the re-read instead.
type fakeRuns struct {
	mu       sync.Mutex
	statuses []string
	err      error
	calls    int
}

func (f *fakeRuns) GetRun(context.Context, string) (store.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return store.Run{}, f.err
	}
	i := min(f.calls, len(f.statuses)-1)
	f.calls++
	return store.Run{ID: testRunID, Status: f.statuses[i]}, nil
}

func (f *fakeRuns) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// sseConfig is the real configuration with every interval shrunk, so an idle
// round is a millisecond rather than five seconds. LoadEvents would refuse
// these values; nothing here goes through it.
func sseConfig() config.Events {
	return config.Events{Heartbeat: 60 * time.Millisecond, ReadBlock: 5 * time.Millisecond}
}

// sseServer builds the real router over a fake reader and a fake run lookup,
// and reports when the handler returns — which is what the leak test asserts.
func sseServer(t *testing.T, runs *fakeRuns, reader *events.FakeReader) (*httptest.Server, <-chan struct{}) {
	t.Helper()

	s := NewServer(Deps{EventReader: reader, Events: sseConfig(), Logger: log.Discard()})
	s.runs = runs

	router := s.Router()
	done := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		router.ServeHTTP(w, r)
		select {
		case done <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(srv.Close)
	return srv, done
}

// subscribe opens the event stream. The caller closes the body.
func subscribe(t *testing.T, srv *httptest.Server, lastEventID string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/runs/"+testRunID+"/events", nil)
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	return resp
}

// readFrames reads until the stream closes, returning what arrived.
func readFrames(t *testing.T, resp *http.Response) string {
	t.Helper()
	var b strings.Builder
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		b.WriteString(sc.Text())
		b.WriteString("\n")
	}
	return b.String()
}

func startedEvent(id string) events.ReadEvent {
	return events.ReadEvent{ID: id, Name: events.RunStarted, Data: []byte(`{"id":"` + testRunID + `"}`)}
}

func finishedEvent(id string) events.ReadEvent {
	return events.ReadEvent{ID: id, Name: events.RunFinished, Data: []byte(`{"status":"SUCCEEDED"}`)}
}

func TestStreamRunEventsRejectsAnIdThisApplicationCouldNotHaveMade(t *testing.T) {
	// The lookup is left empty on purpose: a malformed id must be refused
	// before the handler reaches the store, and a nil statuses slice would
	// panic if it did not.
	srv, _ := sseServer(t, &fakeRuns{}, &events.FakeReader{})

	resp, err := srv.Client().Get(srv.URL + "/api/runs/not-an-id/events")
	if err != nil {
		t.Fatalf("requesting: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// A terminal run is 410 and no stream at all. Replaying one and closing would
// have the browser reconnect for ever, since EventSource retries on every
// close and only a non-200 makes it stop.
func TestStreamRunEventsAnswers410ForATerminalRun(t *testing.T) {
	for _, status := range []string{store.RunSucceeded, store.RunFailed} {
		t.Run(status, func(t *testing.T) {
			reader := &events.FakeReader{Block: time.Millisecond}
			reader.Add(startedEvent("1-0"))
			srv, _ := sseServer(t, &fakeRuns{statuses: []string{status}}, reader)

			resp := subscribe(t, srv, "")
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusGone {
				t.Errorf("status = %d, want 410", resp.StatusCode)
			}
			if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
				t.Errorf("a terminal run opened a stream: Content-Type = %q", ct)
			}
			// The kind stays not_found — the stream genuinely is not there —
			// and the status is what separates "this run is over" from "no
			// such run".
			if body := readFrames(t, resp); !strings.Contains(body, `"code":"not_found"`) {
				t.Errorf("body = %q, want the not_found envelope", body)
			}
		})
	}
}

// The whole stream replays, oldest first, and run.finished ends the request.
func TestStreamRunEventsReplaysThenClosesOnRunFinished(t *testing.T) {
	reader := &events.FakeReader{Block: time.Millisecond}
	reader.Add(startedEvent("1-0"), finishedEvent("2-0"))
	runs := &fakeRuns{statuses: []string{store.RunRunning}}
	srv, done := sseServer(t, runs, reader)

	resp := subscribe(t, srv, "")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	for header, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(header); !strings.HasPrefix(got, want) {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	got := readFrames(t, resp)
	want := "id: 1-0\nevent: run.started\ndata: {\"id\":\"" + testRunID + "\"}\n\n" +
		"id: 2-0\nevent: run.finished\ndata: {\"status\":\"SUCCEEDED\"}\n\n"
	if got != want {
		t.Errorf("stream =\n%q\nwant\n%q", got, want)
	}
	waitForHandler(t, done)
}

// The payload is json.Marshal output, which escapes newlines, so a frame is
// always one data: line — a multi-line one would be read as two events.
func TestStreamRunEventsWritesAPayloadWithANewlineAsOneLine(t *testing.T) {
	reader := &events.FakeReader{Block: time.Millisecond}
	reader.Add(events.ReadEvent{
		ID:   "1-0",
		Name: events.RunFinished,
		Data: []byte(`{"root_cause":"line one\nline two"}`),
	})
	srv, _ := sseServer(t, &fakeRuns{statuses: []string{store.RunRunning}}, reader)

	resp := subscribe(t, srv, "")
	defer resp.Body.Close()

	got := readFrames(t, resp)
	if strings.Count(got, "data: ") != 1 {
		t.Errorf("stream = %q, want exactly one data: line", got)
	}
	if !strings.Contains(got, `data: {"root_cause":"line one\nline two"}`) {
		t.Errorf("stream = %q, want the payload on one line", got)
	}
}

// A reconnecting client resumes after its Last-Event-ID and is sent nothing
// twice.
func TestStreamRunEventsResumesFromLastEventID(t *testing.T) {
	reader := &events.FakeReader{Block: time.Millisecond}
	reader.Add(startedEvent("1-0"), finishedEvent("2-0"))
	srv, _ := sseServer(t, &fakeRuns{statuses: []string{store.RunRunning}}, reader)

	resp := subscribe(t, srv, "1-0")
	defer resp.Body.Close()

	got := readFrames(t, resp)
	if strings.Contains(got, "run.started") {
		t.Errorf("stream = %q, replayed an event the client already had", got)
	}
	if !strings.Contains(got, "id: 2-0") {
		t.Errorf("stream = %q, want the event after the resume point", got)
	}
}

// An id that is not Redis's <ms>-<seq> is treated as absent: the whole stream
// replays, which is wasteful and correct.
func TestStreamRunEventsIgnoresAMalformedLastEventID(t *testing.T) {
	for _, id := range []string{"nonsense", "1-0; DROP", "1", "-1", ""} {
		reader := &events.FakeReader{Block: time.Millisecond}
		reader.Add(startedEvent("1-0"), finishedEvent("2-0"))
		srv, _ := sseServer(t, &fakeRuns{statuses: []string{store.RunRunning}}, reader)

		resp := subscribe(t, srv, id)
		got := readFrames(t, resp)
		resp.Body.Close()

		if !strings.Contains(got, "id: 1-0") {
			t.Errorf("Last-Event-ID %q: stream = %q, want a replay from the start", id, got)
		}
	}
}

// A run whose stream never received run.finished — S8 logs and ignores a
// failed publish — ends on the terminal re-read instead. Without it the client
// would hang until it gave up.
func TestStreamRunEventsClosesWhenTheRunTurnsTerminalWithoutAnEvent(t *testing.T) {
	reader := &events.FakeReader{Block: time.Millisecond}
	reader.Add(startedEvent("1-0"))
	runs := &fakeRuns{statuses: []string{store.RunRunning, store.RunRunning, store.RunSucceeded}}
	srv, done := sseServer(t, runs, reader)

	resp := subscribe(t, srv, "")
	defer resp.Body.Close()

	got := readFrames(t, resp)
	if !strings.Contains(got, "id: 1-0") {
		t.Errorf("stream = %q, want the replayed event", got)
	}
	if strings.Contains(got, "run.finished") {
		t.Errorf("stream = %q, want no synthesised terminal event", got)
	}
	if n := runs.count(); n < 2 {
		t.Errorf("GetRun was called %d times, want the re-read on each idle round", n)
	}
	waitForHandler(t, done)
}

// The heartbeat fires only after silence: a comment frame keeps an idle
// connection alive without inventing a fourth event name.
func TestStreamRunEventsPingsAfterSilence(t *testing.T) {
	reader := &events.FakeReader{Block: time.Millisecond}
	srv, done := sseServer(t, &fakeRuns{statuses: []string{store.RunRunning}}, reader)

	resp := subscribe(t, srv, "")
	defer resp.Body.Close()

	// The first ping is the whole assertion. Reading until it arrives is what
	// proves the handler flushes rather than buffering until it returns.
	r := bufio.NewReader(resp.Body)
	line, err := r.ReadString('\n')
	if err != nil {
		t.Fatalf("reading the first line: %v", err)
	}
	if line != ": ping\n" {
		t.Errorf("first line = %q, want a ping comment", line)
	}

	resp.Body.Close()
	waitForHandler(t, done)
}

// A busy run sends no pings: an event is its own keepalive, which is why the
// heartbeat is a timestamp rather than a ticker.
func TestStreamRunEventsDoesNotPingWhileEventsArrive(t *testing.T) {
	reader := &events.FakeReader{Block: time.Millisecond}
	reader.Add(startedEvent("1-0"))
	srv, done := sseServer(t, &fakeRuns{statuses: []string{store.RunRunning}}, reader)

	resp := subscribe(t, srv, "")
	defer resp.Body.Close()

	// Well under the 60ms heartbeat per event, so a ticker-based heartbeat
	// would still have fired and a timestamp-based one must not.
	for i := 2; i <= 5; i++ {
		time.Sleep(10 * time.Millisecond)
		reader.Add(events.ReadEvent{ID: string(rune('0'+i)) + "-0", Name: events.StepCompleted,
			Data: []byte(`{"step_number":` + string(rune('0'+i)) + `}`)})
	}
	reader.Add(finishedEvent("9-0"))

	got := readFrames(t, resp)
	if strings.Contains(got, ": ping") {
		t.Errorf("stream = %q, want no ping while events were arriving", got)
	}
	if !strings.Contains(got, "run.finished") {
		t.Errorf("stream = %q, want the terminal event", got)
	}
	waitForHandler(t, done)
}

// The handler owns no goroutine, so a client that disconnects mid-stream must
// leave nothing behind. This proves the handler returns; whether go-redis
// released its connection is a different failure.
func TestStreamRunEventsLeavesNothingBehindWhenTheClientDisconnects(t *testing.T) {
	reader := &events.FakeReader{Block: time.Millisecond}
	reader.Add(startedEvent("1-0"))
	srv, done := sseServer(t, &fakeRuns{statuses: []string{store.RunRunning}}, reader)

	before := runtime.NumGoroutine()

	resp := subscribe(t, srv, "")
	r := bufio.NewReader(resp.Body)
	if _, err := r.ReadString('\n'); err != nil {
		t.Fatalf("reading the first frame: %v", err)
	}
	resp.Body.Close()

	waitForHandler(t, done)

	// The server's own connection goroutines wind down asynchronously, so the
	// count is given a moment to settle rather than read once.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Errorf("goroutines: %d before, %d after", before, after)
	}
}

// waitForHandler fails unless the handler has returned.
func waitForHandler(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler did not return")
	}
}
