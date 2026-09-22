package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/events"
	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/store"
)

// errStreamDone ends the request from inside a frame write. It is the reader
// callback's way of saying "that was run.finished", and it never reaches a
// client.
var errStreamDone = errors.New("the run's stream is finished")

// lastEventIDPattern is Redis's entry id. Anything else is treated as absent.
var lastEventIDPattern = regexp.MustCompile(`^\d+-\d+$`)

// streamRunEvents streams one run's timeline as Server-Sent Events.
//
// It replays the stream from the client's resume point, then follows it,
// closing on run.finished, on a run found terminal, or on disconnect. There is
// no goroutine: the handler blocks in the reader itself, so the request is the
// one thing that owns this work and cancelling it is the one way to stop it.
func (s *Server) streamRunEvents(c *gin.Context) {
	runID := c.Param("id")
	if !validID(runID) {
		renderError(c, httpx.NotFound("run %s not found", runID))
		return
	}

	ctx := c.Request.Context()
	run, err := s.runs.GetRun(ctx, runID)
	if err != nil {
		renderError(c, err)
		return
	}
	// A terminal run is 410, never a stream. EventSource reconnects about
	// three seconds after the server closes a stream, so replaying a finished
	// run and closing would loop forever and nothing on the server could break
	// it; a non-200 makes the browser give up. The client already has the
	// whole timeline from GET /api/runs/{id}, which it fetches first.
	if terminal(run.Status) {
		renderError(c, streamGone(runID))
		return
	}

	s.stream(c, runID)
}

// stream writes the event stream. Everything before it has already decided
// that there is one to write.
func (s *Server) stream(c *gin.Context, runID string) {
	ctx := c.Request.Context()
	w := c.Writer
	rc := http.NewResponseController(w)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// A proxy that buffered this would hold every frame until the run ended,
	// which is the failure this endpoint exists to prevent.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	w.WriteHeaderNow()
	lastWrite := time.Now()
	if err := s.flush(rc); err != nil {
		return
	}

	after := lastEventID(c.Request)
	var wrote bool

	send := func(ev events.ReadEvent) error {
		if err := s.writeFrame(rc, w, ev); err != nil {
			return err
		}
		after, lastWrite, wrote = ev.ID, time.Now(), true
		if ev.Name == events.RunFinished {
			return errStreamDone
		}
		return nil
	}

	if err := s.deps.EventReader.Replay(ctx, runID, after, send); err != nil {
		s.streamEnded(ctx, runID, err)
		return
	}

	for ctx.Err() == nil {
		wrote = false
		if err := s.deps.EventReader.Follow(ctx, runID, after, send); err != nil {
			s.streamEnded(ctx, runID, err)
			return
		}
		// An event is its own keepalive, and it may also have been the last
		// one this run will produce — either way there is nothing to check.
		if wrote {
			continue
		}

		// The stream cannot be trusted to announce the end: a failed publish
		// is logged and ignored by design, so a terminal run's stream may hold
		// no run.finished. MySQL is asked instead, once per idle round.
		if s.finished(ctx, runID) {
			return
		}

		// A timestamp rather than a ticker: the handler is blocked in the
		// reader most of the time, and a ticker would fire the moment it
		// returned. Comparing against the last byte written is exact, and a
		// busy run sends no pings at all.
		if time.Since(lastWrite) >= s.deps.Events.Heartbeat {
			if err := s.writeComment(rc, w, "ping"); err != nil {
				return
			}
			lastWrite = time.Now()
		}
	}
}

// finished reports whether the run has reached a terminal state.
//
// A failed read is logged and treated as "not yet". MySQL being briefly
// unreachable should cost a run's live timeline no more than it costs
// /readyz's opinion of this process, and the loop's own condition still ends
// the request when the client goes away.
func (s *Server) finished(ctx context.Context, runID string) bool {
	run, err := s.runs.GetRun(ctx, runID)
	if err != nil {
		if ctx.Err() == nil {
			s.deps.Logger.WarnContext(ctx, "could not re-read a streaming run",
				"run_id", runID, "error", err)
		}
		return false
	}
	return terminal(run.Status)
}

// streamEnded logs a stream that ended on an error rather than on its run.
//
// The headers are already sent, so there is no status code left and no
// synthetic error event either: the vocabulary is S8's three names, and a
// fourth would have to be rendered by a front end that has a better source for
// the same fact. The client reconnects, or takes its answer from
// GET /api/runs/{id}.
func (s *Server) streamEnded(ctx context.Context, runID string, err error) {
	if errors.Is(err, errStreamDone) || errors.Is(err, context.Canceled) {
		return
	}
	s.deps.Logger.WarnContext(ctx, "a run's event stream ended early",
		"run_id", runID, "error", err)
}

// writeFrame writes one event as id:/event:/data: and a blank line.
//
// The payload is json.Marshal output, which escapes newlines, so data is
// always a single line.
func (s *Server) writeFrame(rc *http.ResponseController, w gin.ResponseWriter, ev events.ReadEvent) error {
	var b strings.Builder
	b.WriteString("id: ")
	b.WriteString(ev.ID)
	b.WriteString("\nevent: ")
	b.WriteString(ev.Name)
	b.WriteString("\ndata: ")
	b.Write(ev.Data)
	b.WriteString("\n\n")
	return s.write(rc, w, b.String())
}

// writeComment writes an SSE comment, which a client ignores and a proxy
// counts as traffic.
func (s *Server) writeComment(rc *http.ResponseController, w gin.ResponseWriter, text string) error {
	return s.write(rc, w, ": "+text+"\n\n")
}

// write extends the deadline, writes, and flushes.
//
// cmd/api sets WriteTimeout, and a run outlasts it, so without the extension
// the server would cut its own stream. The deadline is now + 2 × the
// heartbeat: the gap between two writes is bounded by the heartbeat, so twice
// it states the invariant directly. It is extended per write rather than
// cleared once, because a client that stopped reading would then block this
// handler in Write forever.
func (s *Server) write(rc *http.ResponseController, w gin.ResponseWriter, frame string) error {
	if err := rc.SetWriteDeadline(time.Now().Add(2 * s.deps.Events.Heartbeat)); err != nil {
		if !errors.Is(err, http.ErrNotSupported) {
			return err
		}
	}
	if _, err := w.WriteString(frame); err != nil {
		return err
	}
	return s.flush(rc)
}

// flush pushes what has been written to the client.
//
// Without it gin buffers the response and nothing reaches the browser until
// the handler returns, which is the whole failure this endpoint exists to
// prevent. http.ErrNotSupported is tolerated so a test can drive the handler
// with an httptest.ResponseRecorder.
func (s *Server) flush(rc *http.ResponseController) error {
	if err := rc.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	return nil
}

// lastEventID reads the client's resume point.
//
// An id that is not Redis's <ms>-<seq> is treated as absent and the whole
// stream is replayed, which is wasteful and correct: the browser already drops
// what it has by step number within the current attempt.
func lastEventID(r *http.Request) string {
	v := r.Header.Get("Last-Event-ID")
	if !lastEventIDPattern.MatchString(v) {
		return ""
	}
	return v
}

// terminal reports whether a run has stopped for good.
func terminal(status string) bool {
	return status == store.RunSucceeded || status == store.RunFailed
}
