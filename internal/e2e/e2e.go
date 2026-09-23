// Package e2e runs a whole investigation: inject a fault into the Incident
// Lab, file an incident through the API, let the agent work, and report what
// it did.
//
// It asserts nothing. M31's test makes the assertions and M32 aggregates the
// same outcomes over many runs, so anything decided here would have to be
// undecided there.
//
// Everything goes through the API a browser uses. Reaching into MySQL for the
// answer would test something the product does not do.
package e2e

import (
	"encoding/json"
	"time"
)

// Scenario is one end-to-end case: a fault, the incident a responder would
// file about it, and what a correct diagnosis names.
type Scenario struct {
	// Name identifies the case in a report.
	Name string

	// Fault is the internal/lab/scenario name to inject. Empty means none,
	// which is the healthy control: the right diagnosis is that nothing is
	// wrong.
	Fault string

	Title       string
	Description string

	// Service is what the incident says is affected, which is what a responder
	// would guess rather than what is true.
	Service string

	// ExpectService is what final_result.affected_service should name. For the
	// healthy control it is empty, because there is nothing to name.
	ExpectService string

	// Expect is how the diagnosis is marked. The zero value marks nothing,
	// which is what M31 wants: it asserts in the test instead.
	Expect Expectation
}

// Evidence is one citation, as the API reports it.
//
// It hangs off the step it points at rather than the finish step that made it:
// the run's citations are resolved to step ids when the diagnosis is written,
// and the API groups them by that id.
type Evidence struct {
	StepID     string  `json:"step_id"`
	ToolCallID *string `json:"tool_call_id"`
	SourceType string  `json:"source_type"`
	SourceRef  string  `json:"source_ref"`
	DocumentID *string `json:"document_id"`
	Note       *string `json:"note"`
}

// Step is one recorded step, as the API reports it.
//
// The whole trajectory is kept, not just the verdict: "wrong" and "wrong, but
// it queried the right metric at step 3 and misread it" are different
// problems, and only the second says what to change.
type Step struct {
	ID         string
	Number     int
	ActionType string
	Status     string
	Tool       string
	ToolStatus string
	Error      string
	Duration   time.Duration
	Evidence   []Evidence
}

// Outcome is everything one run produced.
type Outcome struct {
	Scenario   string
	IncidentID string
	RunID      string

	Status     string
	StopReason string
	Error      string

	Steps            []Step
	StepCount        int
	ToolCallCount    int
	PromptTokens     int
	CompletionTokens int

	// FinalResult is the finish arguments as they were given, and the two
	// fields below are read out of it for convenience.
	FinalResult     json.RawMessage
	AffectedService string
	RootCause       string

	// Elapsed is wall time from filing the incident to the terminal run, so it
	// includes the queue and the claim as well as the investigation.
	Elapsed time.Duration
}

// Retrievals counts the steps that searched the knowledge base.
func (o Outcome) Retrievals() int { return o.countActions("retrieve") }

// ToolCalls counts the steps that called an ops-mcp tool.
func (o Outcome) ToolCalls() int { return o.countActions("tool_call") }

func (o Outcome) countActions(kind string) int {
	n := 0
	for _, s := range o.Steps {
		if s.ActionType == kind {
			n++
		}
	}
	return n
}

// EvidenceCount is how many citations the diagnosis resolved onto steps.
func (o Outcome) EvidenceCount() int {
	n := 0
	for _, s := range o.Steps {
		n += len(s.Evidence)
	}
	return n
}

// ToolsUsed returns the distinct tools the run called, in the order first
// called.
func (o Outcome) ToolsUsed() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range o.Steps {
		if s.Tool == "" || seen[s.Tool] {
			continue
		}
		seen[s.Tool] = true
		out = append(out, s.Tool)
	}
	return out
}

// DefaultScenarios are the cases M31 can run. M32 extends this set; the two
// here are the fault the corpus documents best and the control that makes an
// accuracy number mean something.
func DefaultScenarios() []Scenario {
	return []Scenario{
		{
			Name:  "payment-latency",
			Fault: "payment-latency",
			Title: "Checkout is timing out on payment",
			Description: "POST /orders on checkout-service has been returning 504 for the last " +
				"ten minutes. payment-service still answers 200 but slowly. No deploy went out " +
				"today and no configuration changed.",
			Service:       "checkout-service",
			ExpectService: "payment-service",
		},
		{
			Name:  "baseline",
			Fault: "",
			Title: "Possible slowdown on checkout",
			Description: "A customer reported one slow checkout. Nothing is alerting and the " +
				"dashboards look normal. Please confirm whether anything is actually wrong.",
			Service:       "checkout-service",
			ExpectService: "",
		},
	}
}
