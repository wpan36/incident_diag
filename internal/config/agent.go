package config

import (
	"fmt"
	"time"
)

// BytesPerToken is the worst case this project converts bytes to tokens with.
//
// It is a constant here rather than a call into internal/ingest, which has the
// real estimator in tokens.go: this package is the bottom of the dependency
// graph and ingest depends on it through store, so importing it would be a
// cycle. Four bytes per token is the "other" branch of that estimator, which
// is the cheaper of its two ratios and therefore the safe one for a check that
// must not pass a configuration the runtime then rejects.
//
// internal/agent estimates a prompt with this same constant, so the invariant
// below and the bound at run time cannot disagree.
const BytesPerToken = 4

// observationCapBytes mirrors summary.LimitBytes, the 8 KiB cap the data model
// spec fixes on one observation.
//
// Duplicated rather than imported, to keep this package free of project
// imports. The value is fixed by the schema — observation_summary is written
// through that cap — so the two cannot drift without a migration noticing.
const observationCapBytes = 8 << 10

// promptOverheadBytes is everything in a prompt that the observations do not
// cover: the incident, the system prompt and the tool-call arguments.
//
// The API caps an incident description at 8192 characters, which is 33 KiB of
// UTF-8 at worst, and the rest is margin. Without it the run-time token bound
// could fire on a configuration LoadAgent had just accepted, and the forced
// finish would then be built from the same over-budget prompt.
const promptOverheadBytes = 40 << 10

// Agent is the bounded agent loop's budget.
//
// All four are recorded on agent_runs when a run is created rather than read
// back from here while it executes, so a run stays interpretable after the
// configuration changes.
type Agent struct {
	// MaxSteps bounds how many iterations a run may start. The forced finish
	// that a bound triggers takes one more, so a run's step count can exceed
	// this by one.
	MaxSteps int

	// MaxToolCalls bounds MCP tool invocations. Retrieval does not count
	// toward it: search_knowledge writes no tool_calls row, and MaxSteps is
	// what bounds retrieval.
	MaxToolCalls int

	// MaxRunDuration is wall-clock and is checked between steps. It is
	// deliberately not a deadline on the run's context: the forced finish
	// would then run on an already-expired context and could never succeed.
	MaxRunDuration time.Duration

	// MaxPromptTokens is a ceiling on one assembled prompt, not on a run's
	// cumulative spend. The invariant below means it cannot fire under a valid
	// configuration; it is the safety net that makes "the context is never
	// pruned" safe rather than lucky.
	MaxPromptTokens int
}

// LoadAgent reads the agent's budget from the environment, reporting every
// problem it finds at once.
//
// It is the second loader that validates a relationship between two of its own
// values, after LoadReconcile. The agent never prunes its context, and the
// reason that is safe is arithmetic: MaxSteps observations of 8 KiB plus the
// incident and the prompt have to fit inside MaxPromptTokens. Raising
// AGENT_MAX_STEPS without raising AGENT_MAX_PROMPT_TOKENS would silently
// invalidate that reasoning, so it is refused at startup instead.
func LoadAgent() (Agent, error) {
	var e env

	a := Agent{
		MaxSteps:        e.optionalInt("AGENT_MAX_STEPS", 8),
		MaxToolCalls:    e.optionalInt("AGENT_MAX_TOOL_CALLS", 12),
		MaxRunDuration:  e.optionalDuration("AGENT_MAX_RUN_DURATION", 5*time.Minute),
		MaxPromptTokens: e.optionalInt("AGENT_MAX_PROMPT_TOKENS", 60000),
	}

	if a.MaxSteps < 1 {
		e.fail("AGENT_MAX_STEPS must be at least 1, got %d", a.MaxSteps)
	}
	if a.MaxToolCalls < 1 {
		e.fail("AGENT_MAX_TOOL_CALLS must be at least 1, got %d", a.MaxToolCalls)
	}
	if a.MaxRunDuration <= 0 {
		e.fail("AGENT_MAX_RUN_DURATION must be greater than zero")
	}
	if a.MaxPromptTokens < 1 {
		e.fail("AGENT_MAX_PROMPT_TOKENS must be at least 1, got %d", a.MaxPromptTokens)
	}

	if a.MaxSteps >= 1 && a.MaxPromptTokens >= 1 {
		if worst := a.WorstCasePromptTokens(); worst > a.MaxPromptTokens {
			e.fail("AGENT_MAX_STEPS (%d) needs at least %d prompt tokens in the worst case, "+
				"but AGENT_MAX_PROMPT_TOKENS is %d; the agent never prunes its context, so "+
				"raise the token ceiling or lower the step count",
				a.MaxSteps, worst, a.MaxPromptTokens)
		}
	}

	if err := e.err(); err != nil {
		return Agent{}, err
	}
	return a, nil
}

// WorstCasePromptTokens is the largest prompt MaxSteps can produce: every step
// answering with a full 8 KiB observation, plus the fixed overhead.
func (a Agent) WorstCasePromptTokens() int {
	bytes := a.MaxSteps*observationCapBytes + promptOverheadBytes
	return (bytes + BytesPerToken - 1) / BytesPerToken
}

// String renders the configuration for startup logging.
func (a Agent) String() string {
	return fmt.Sprintf("max_steps=%d max_tool_calls=%d max_run_duration=%s max_prompt_tokens=%d",
		a.MaxSteps, a.MaxToolCalls, a.MaxRunDuration, a.MaxPromptTokens)
}
