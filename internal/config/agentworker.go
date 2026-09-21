package config

import (
	"fmt"
	"time"
)

// RebalanceMargin is what the consumer's rebalance timeout adds on top of the
// worst case a run can legitimately take.
//
// Its job is the retry back-off. internal/llm sleeps between attempts — up to
// ten seconds each, so forty seconds across the two model calls the worst case
// counts — and no per-attempt timeout covers that. One minute, the value
// cmd/ingestion-worker already uses, absorbs it with room left, which is why
// this stays a constant instead of becoming another variable.
const RebalanceMargin = time.Minute

// AgentWorker is what cmd/agent-worker needs and no other binary does.
//
// The Redis settings are not here: two binaries need those, so they have a
// loader of their own (LoadEvents).
type AgentWorker struct {
	// RunLease is how long a claim holds a run before the sweep may reclaim
	// it. It is the same value ClaimRun is called with, or the sweep would
	// re-enqueue runs the claim then refuses.
	RunLease time.Duration

	// RunMaxAttempts is how many times a run may be claimed before it is left
	// RUNNING for a human. Attempts are counted by the claim, mirroring
	// INGEST_MAX_ATTEMPTS, so three permits two restarts.
	RunMaxAttempts int

	// ToolServerURL is ops-mcp. The worker refuses to start if it cannot
	// reach it: a worker that started without tools would claim runs it
	// cannot investigate.
	ToolServerURL string

	// ToolTimeout is mcpclient's per-call deadline.
	ToolTimeout time.Duration

	// WorstCase is the longest one run can legitimately take. It is derived
	// rather than configured; see LoadAgentWorker.
	WorstCase time.Duration

	// RebalanceTimeout is WorstCase + RebalanceMargin, passed through
	// mq.WithRebalanceTimeout. Derived, as internal/mq requires: a handler
	// that outlasts it gets the member evicted and the message redelivered.
	RebalanceTimeout time.Duration
}

// LoadAgentWorker reads the agent worker's configuration and checks the one
// relationship that matters: the lease has to outlast a run.
//
// S7's accepted limitation is that bounds are checked between steps, so a run
// may overrun MaxRunDuration by one full step plus the forced finish. A step
// is not only a model call: it is a model call and either an MCP tool call or
// a retrieval. The forced finish is a model call alone.
//
//	llmStep      = LLM_TIMEOUT × (1 + LLM_MAX_RETRIES)                       // 3m
//	retrieval    = EMBED_TIMEOUT × (1 + EMBED_MAX_RETRIES) + SEARCH_TIMEOUT  // 2m10s
//	stepOverhead = max(AGENT_TOOL_TIMEOUT, retrieval)                        // 2m10s
//	worstCase    = AGENT_MAX_RUN_DURATION + 2 × llmStep + stepOverhead       // 13m10s
//
// Counting only the model calls would give eleven minutes for a run that can
// legitimately take thirteen, and a lease sized from that figure is the
// failure this check exists to prevent: the sweep reclaims a run that is still
// working, the second attempt deletes rows the first is still writing, and the
// two collide on UNIQUE (run_id, step_number) — which internal/store
// classifies as a bug, not a conflict, so the run ends FAILED with a 500 in
// the log.
//
// What the figure does not count: the MySQL writes agentrun.ReportStep makes
// after each step. They run on the run's context, which carries no deadline on
// purpose, so a row-lock wait alone can consume the headroom below. The lease
// is therefore a well-sized estimate and not a proof, which is why FinishRun
// is conditional on the attempt as well as the status — an overrun then costs
// a discarded outcome rather than a corrupted row.
//
// The formula spans four loaders, so this one takes their results as
// arguments rather than re-reading their variables. The ordering it enforces
// is AGENT_MAX_RUN_DURATION < worstCase < rebalance timeout < RUN_LEASE, which
// at the defaults is 5m < 13m10s < 14m10s < 15m. Fifty seconds of headroom is
// thin, and deliberately so: raising EMBED_MAX_RETRIES or LLM_TIMEOUT should
// fail at startup with a message naming the lease, not reclaim live runs.
func LoadAgentWorker(agent Agent, llm LLM, embedding Embedding, search Search) (AgentWorker, error) {
	var e env

	w := AgentWorker{
		RunLease:       e.optionalDuration("RUN_LEASE", 15*time.Minute),
		RunMaxAttempts: e.optionalInt("RUN_MAX_ATTEMPTS", 3),
		ToolServerURL:  e.requiredString("AGENT_TOOL_SERVER_URL"),
		ToolTimeout:    e.optionalDuration("AGENT_TOOL_TIMEOUT", 30*time.Second),
	}

	if w.RunLease <= 0 {
		e.fail("RUN_LEASE must be greater than zero")
	}
	if w.RunMaxAttempts < 1 {
		e.fail("RUN_MAX_ATTEMPTS must be at least 1, got %d", w.RunMaxAttempts)
	}
	if w.ToolTimeout <= 0 {
		e.fail("AGENT_TOOL_TIMEOUT must be greater than zero")
	}
	if w.ToolServerURL != "" {
		if err := checkAbsoluteHTTP(w.ToolServerURL); err != nil {
			e.fail("AGENT_TOOL_SERVER_URL %v", err)
		}
	}

	w.WorstCase = worstCaseRunDuration(agent, llm, embedding, search, w.ToolTimeout)
	w.RebalanceTimeout = w.WorstCase + RebalanceMargin

	if w.RunLease > 0 && w.RunLease <= w.RebalanceTimeout {
		e.fail("RUN_LEASE (%s) must exceed %s: a run can legitimately take %s "+
			"(AGENT_MAX_RUN_DURATION plus one overrunning step and the forced finish), "+
			"and the consumer's rebalance timeout adds %s on top of that. "+
			"Raise RUN_LEASE, or lower AGENT_MAX_RUN_DURATION, LLM_TIMEOUT, "+
			"LLM_MAX_RETRIES, EMBED_TIMEOUT, EMBED_MAX_RETRIES or SEARCH_TIMEOUT",
			w.RunLease, w.RebalanceTimeout, w.WorstCase, RebalanceMargin)
	}

	if err := e.err(); err != nil {
		return AgentWorker{}, err
	}
	return w, nil
}

// worstCaseRunDuration is the derivation LoadAgentWorker documents.
//
// It is a function rather than a single AGENT_STEP_OVERHEAD variable because
// nothing would tie such a variable to EMBED_TIMEOUT or EMBED_MAX_RETRIES, so
// raising either would leave a stale figure behind — and the consequence of a
// stale figure is the rebalance evicting a worker mid-run.
func worstCaseRunDuration(agent Agent, llm LLM, embedding Embedding, search Search,
	toolTimeout time.Duration) time.Duration {

	llmStep := llm.Timeout * time.Duration(1+llm.MaxRetries)
	retrieval := embedding.Timeout*time.Duration(1+embedding.MaxRetries) + search.Timeout

	stepOverhead := toolTimeout
	if retrieval > stepOverhead {
		stepOverhead = retrieval
	}
	// Two model calls: the step that overran the bound, and the forced finish.
	return agent.MaxRunDuration + 2*llmStep + stepOverhead
}

// String renders the configuration for startup logging.
func (w AgentWorker) String() string {
	return fmt.Sprintf("run_lease=%s run_max_attempts=%d tool_server=%s tool_timeout=%s "+
		"worst_case=%s rebalance_timeout=%s",
		w.RunLease, w.RunMaxAttempts, w.ToolServerURL, w.ToolTimeout,
		w.WorstCase, w.RebalanceTimeout)
}
