// Package agentrun consumes agent.runs.v1 and executes one investigation per
// message: claim the run, run the bounded loop, write its outcome.
//
// It is internal/agent's one seam onto infrastructure. That package performs
// no I/O beyond the model and the tools; the steps it produces come here,
// where they become rows in MySQL and events on a Redis stream. Keeping the
// two apart is what makes the loop testable with no database, no broker and
// no provider.
//
// The frame around the work — decode, claim, terminal write — mirrors
// internal/ingest exactly, and for the same reason: the claim is a conditional
// UPDATE, so a redelivery finds the work already taken, and FinishRun is
// conditional on the run still being RUNNING under this attempt, so neither a
// late duplicate nor an attempt the lease has superseded can overwrite the row.
//
// One rule runs through all of it, from ADR 0001: the MySQL row is written
// before its event is published, and a failed publish is logged and ignored.
// A reconnecting client reads GET /api/runs/{id} and then subscribes, so it
// can never see an event for something that is not yet persisted; a lost
// event costs a live timeline, never a fact.
package agentrun

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/wpan36/incident_diag/internal/agent"
	"github.com/wpan36/incident_diag/internal/events"
	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/store"
	"github.com/wpan36/incident_diag/internal/wire"
)

// terminalTimeout bounds the writes that record how a run ended.
//
// They run on a context detached from the consumer's, for the reason
// internal/ingest details: the most important thing a handler whose context
// has just been cancelled can do is say what happened. A cancelled FinishRun
// would leave the row RUNNING with nothing but the lease to rescue it.
//
// It does not apply to a run cancelled by shutdown, which deliberately writes
// nothing terminal — see Handle.
const terminalTimeout = 10 * time.Second

// Deps is everything the handler needs, mirroring ingest.Deps.
//
// The agent's own dependencies are here rather than a built *agent.Agent,
// because the agent needs this package's ReportStep and this package needs the
// agent: constructing the agent inside NewHandler is what breaks that circle
// without a setter.
type Deps struct {
	Store  *store.Store
	Events events.Publisher

	// The agent's dependencies. LLM holds its own model, which is why
	// agent_runs.model records what the API believed rather than instructing
	// the worker.
	LLM       llm.Chatter
	Knowledge *agent.Knowledge
	Tools     agent.ToolServer

	// Lease must be the same value the run reconciler uses, or the sweep would
	// re-enqueue runs the claim then refuses.
	Lease time.Duration

	Logger *slog.Logger
}

// Handler processes one agent.runs.v1 record.
type Handler struct {
	deps  Deps
	agent *agent.Agent
}

// NewHandler builds the handler and the agent behind it.
func NewHandler(deps Deps) *Handler {
	h := &Handler{deps: deps}
	h.agent = agent.New(agent.Deps{
		LLM:       deps.LLM,
		Knowledge: deps.Knowledge,
		Tools:     deps.Tools,
		Report:    h.ReportStep,
		Logger:    deps.Logger,
	})
	return h
}

// Handle is an mq.Handler.
//
// It returns an error only when something outside this message is wrong —
// MySQL unreachable, most likely. Every outcome about the message itself is
// recorded in MySQL and returns nil, because the offset is committed either
// way and a returned error would only add noise.
func (h *Handler) Handle(ctx context.Context, rec mq.Record) error {
	msg, err := mq.Decode[mq.RunMessage](rec.Value)
	if err != nil {
		// Unlike a document, a run needs nothing recorded against it here.
		// Both decoding failures leave the row PENDING, which is the
		// reconciler's never-enqueued category — and what the sweep produces
		// is a fresh, well-formed message this build understands, so even an
		// unknown schema version recovers.
		h.deps.Logger.ErrorContext(ctx, "run message could not be decoded",
			"topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset,
			"run_id", msg.RunID, "error", err)
		return nil
	}

	// Every record this handler writes from here on carries the run id, the
	// way the API's middleware carries the request id.
	ctx = log.WithRunID(ctx, msg.RunID)

	claimed, err := h.deps.Store.ClaimRun(ctx, msg.RunID, h.deps.Lease)
	if err != nil {
		return fmt.Errorf("claim run %s: %w", msg.RunID, err)
	}
	if !claimed {
		// Already RUNNING under another attempt, or already terminal. Nothing
		// is published: the message is a redelivery of work that is done or in
		// hand.
		return h.explainRefusedClaim(ctx, msg.RunID)
	}
	return h.investigate(ctx, msg.RunID)
}

// investigate runs one claimed run.
//
// There is deliberately no deadline on this context. MaxRunDuration is
// wall-clock and checked between steps (S7): as a deadline it would expire
// before the forced finish, which is the one call that turns a bound into a
// diagnosis. What bounds the handler instead is the budget on the row, and
// what bounds the damage of that being wrong is RUN_LEASE.
func (h *Handler) investigate(ctx context.Context, runID string) error {
	// Re-read rather than publishing the row as it was before the claim, which
	// would put PENDING, a stale attempts and a null started_at into
	// run.started. ClaimRun returns (bool, error) and not the row, because
	// false is a legal outcome rather than a failure.
	run, err := h.deps.Store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("read run %s: %w", runID, err)
	}
	inc, err := h.deps.Store.GetIncident(ctx, run.IncidentID)
	if err != nil {
		return fmt.Errorf("read incident %s of run %s: %w", run.IncidentID, runID, err)
	}

	// The stream cannot join the claim's transaction — Redis is not in it — so
	// it is emptied immediately after. A failure is logged and ignored like
	// any other Redis failure; the worst case is the spliced timeline this
	// ordering is trying to avoid, on a restart of a run whose Redis was
	// already failing.
	if err := h.deps.Events.Trim(ctx, runID); err != nil {
		h.deps.Logger.ErrorContext(ctx, "could not empty the run's event stream", "error", err)
	}
	h.publish(ctx, runID, events.RunStarted, wire.NewRun(run))

	started := time.Now()
	h.deps.Logger.InfoContext(ctx, "claimed run",
		"incident_id", run.IncidentID, "attempt", run.Attempts,
		"max_steps", run.MaxSteps, "max_tool_calls", run.MaxToolCalls)

	outcome, err := h.agent.Run(ctx, agent.Run{
		ID:       run.ID,
		Incident: incidentFor(inc),
		// From the row, not from configuration: a run keeps the bounds it was
		// created with even if the environment has moved on.
		Budget: agent.Budget{
			MaxSteps:        run.MaxSteps,
			MaxToolCalls:    run.MaxToolCalls,
			MaxRunDuration:  time.Duration(run.MaxDurationSeconds) * time.Second,
			MaxPromptTokens: run.MaxPromptTokens,
		},
	})
	if err != nil {
		// agent.Run returns an error for cancellation and nothing else, and
		// the only thing that cancels this context is shutdown. Nothing
		// terminal is written: the run stays RUNNING, its offset uncommitted,
		// and the lease reclaims it (ADR 0009). The cost is that the incident
		// answers 409 until the lease expires.
		h.deps.Logger.InfoContext(ctx, "run cancelled; the lease will restart it",
			"duration", time.Since(started), "error", err)
		return nil
	}

	return h.finish(ctx, runID, run.Attempts, outcome, started)
}

// finish writes the terminal row and publishes run.finished.
//
// attempt is the claim this handler is working under, read back from the row
// after ClaimRun. It is part of FinishRun's condition so that an attempt the
// lease has already superseded cannot terminate a run another worker is still
// running.
func (h *Handler) finish(ctx context.Context, runID string, attempt int,
	out store.RunOutcome, started time.Time) error {

	// Detached from the consumer's context for the reason terminalTimeout
	// states. It is not detached from a cancelled run: that path returns above
	// without reaching here.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalTimeout)
	defer cancel()

	written, err := h.deps.Store.FinishRun(writeCtx, runID, attempt, out)
	if err != nil {
		return fmt.Errorf("finish run %s: %w", runID, err)
	}
	if !written {
		// Per the store's contract, false means the run is no longer this
		// attempt's: either another attempt already finished it, or the lease
		// expired and a later claim took it over while this one was working.
		// Either way the outcome computed here describes work someone else has
		// superseded, so nothing is written and nothing is published.
		h.deps.Logger.WarnContext(ctx, "run is no longer this attempt's; its outcome is discarded",
			"attempt", attempt, "status", out.Status, "stop_reason", out.StopReason)
		return nil
	}

	h.deps.Logger.InfoContext(ctx, "run finished",
		"status", out.Status, "stop_reason", out.StopReason,
		"steps", out.StepCount, "tool_calls", out.ToolCallCount,
		"prompt_tokens", out.PromptTokens, "completion_tokens", out.CompletionTokens,
		"duration", time.Since(started))

	// Re-read for the same reason run.started does: FinishRun returns whether
	// it wrote, not what it wrote.
	run, err := h.deps.Store.GetRun(writeCtx, runID)
	if err != nil {
		// The row is correct and terminal; only the event is lost, which is
		// the same cost as a failed publish.
		h.deps.Logger.ErrorContext(ctx, "could not read the finished run to publish it", "error", err)
		return nil
	}
	h.publish(writeCtx, runID, events.RunFinished, wire.NewRun(run))
	return nil
}

// explainRefusedClaim turns a false claim into a log line that says which of
// the two possible reasons it was, mirroring internal/ingest.
//
// A refused claim is either another attempt already holding the work — the
// ordinary case under at-least-once delivery — or a message naming a row that
// does not exist, which is a real problem and would otherwise be invisible.
// The extra read only happens on this path, which is the rare one.
func (h *Handler) explainRefusedClaim(ctx context.Context, runID string) error {
	run, err := h.deps.Store.GetRun(ctx, runID)
	switch {
	case err == nil:
		h.deps.Logger.DebugContext(ctx, "run already claimed or already terminal, skipping",
			"status", run.Status)
		return nil
	case httpx.KindOf(err) == httpx.KindNotFound:
		h.deps.Logger.ErrorContext(ctx, "message names a run that does not exist")
		return nil
	default:
		return fmt.Errorf("look up run %s: %w", runID, err)
	}
}

// publish sends one event and swallows the failure.
//
// Every caller in this package does this, which is why it is a helper rather
// than a repeated three lines: the row is already written, so a publish
// failure costs a live timeline and nothing else. The user sees a timeline
// that simply stops, and a refresh fixes it.
func (h *Handler) publish(ctx context.Context, runID, name string, payload any) {
	if err := h.deps.Events.Publish(ctx, runID, events.Event{Name: name, Payload: payload}); err != nil {
		h.deps.Logger.ErrorContext(ctx, "could not publish a run event", "event", name, "error", err)
	}
}

// incidentFor converts a stored incident into what the loop reads.
func incidentFor(in store.Incident) agent.Incident {
	i := agent.Incident{
		ID:          in.ID,
		Title:       in.Title,
		Description: in.Description,
		CreatedAt:   in.CreatedAt,
	}
	if in.Service != nil {
		i.Service = *in.Service
	}
	return i
}
