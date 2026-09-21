// Package agent is the bounded investigation loop: the part of this project
// worth understanding, written by hand rather than delegated to a framework.
//
// One model call per step, one tool call per step, and four bounds that stop
// it starting new work. Hitting a bound is not a failure — the loop makes one
// final call offering only finish, so a truncated investigation still ends
// with a real partial diagnosis.
//
// The loop performs no I/O of its own beyond the model and the tools. Steps
// are handed to a callback that persists them and publishes their events, so
// the whole of this package is testable against a scripted llm.Fake and fake
// tools, with no database, no broker and no provider.
package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wpan36/incident_diag/internal/mcpclient"
	"github.com/wpan36/incident_diag/internal/search"
)

// Tool names the loop treats specially. Everything else in the list is
// discovered from ops-mcp and passed through untouched.
const (
	// ToolSearchKnowledge is this system's own retrieval, run in process.
	ToolSearchKnowledge = "search_knowledge"

	// ToolFinish ends the run. It is not an MCP tool, but presenting it
	// identically means the model has one mechanism rather than "call a tool,
	// or else answer in prose".
	ToolFinish = "finish"
)

// Incident is what a run investigates.
type Incident struct {
	ID          string
	Title       string
	Description string
	Service     string
	CreatedAt   time.Time
}

// Budget is the bounds that applied to a run.
//
// It is passed in rather than read from configuration inside the loop,
// because agent_runs records the budget when the run is created: a run stays
// interpretable after the configuration changes.
type Budget struct {
	MaxSteps        int
	MaxToolCalls    int
	MaxRunDuration  time.Duration
	MaxPromptTokens int
}

// Run is one investigation.
type Run struct {
	ID       string
	Incident Incident
	Budget   Budget
}

// Action is the structured decision the model returned, as agent_steps.action
// stores it. Never the reasoning behind it: the schema has nowhere to put
// chain-of-thought, which is deliberate.
type Action struct {
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolCall is the tool_calls row a step produced.
//
// Result is the full text; the store truncates it and records the original
// length, so those two can never disagree.
type ToolCall struct {
	Name      string
	Arguments json.RawMessage
	Status    string
	Result    string
	Error     string
	Duration  time.Duration
}

// Evidence is one citation from finish, already resolved against the steps
// that actually happened. The loop resolves them itself so that the callback
// only has to write rows.
type Evidence struct {
	StepID     string
	ToolCallID string
	SourceType string
	SourceRef  string
	DocumentID string
	Summary    string
	Note       string
}

// Citation is one item of finish's evidence array, as the model wrote it.
//
// DocumentID is optional and is kept only when it appears among the cited
// step's hits. Without it the evidence table cannot answer the question it
// was built for — which documents actually get cited — because a retrieval
// step returns several.
type Citation struct {
	Step       int    `json:"step"`
	Note       string `json:"note"`
	DocumentID string `json:"document_id,omitempty"`
}

// FinalResult is finish's arguments. agent_runs.final_result holds them as
// they were given.
type FinalResult struct {
	RootCause       string     `json:"root_cause"`
	AffectedService string     `json:"affected_service"`
	NextActions     []string   `json:"next_actions"`
	Evidence        []Citation `json:"evidence"`
}

// Observation is what executing one action produced.
//
// Text is the full, untruncated observation: the context builder caps it for
// the prompt and the store caps it for the audit row, both with the same
// helper, so neither has to trust the other.
type Observation struct {
	Text  string
	Error string

	// ToolCall is set when the action invoked an MCP tool.
	ToolCall *ToolCall

	// Hits is set when the action was a retrieval. The loop keeps them until
	// the run ends: they are what validates a cited document_id and what
	// fills an evidence row with the one hit the model cited rather than all
	// five.
	Hits []search.Result

	// Query is the retrieval's query, used as a citation's source_ref when no
	// individual hit was identified.
	Query string
}

// Step is one iteration, as the loop reports it.
type Step struct {
	RunID      string
	StepNumber int
	ActionType string
	Action     json.RawMessage
	// Observation is the full text. store.CreateStep truncates it.
	Observation string
	Status      string
	Error       string
	Duration    time.Duration

	// ToolCall is set when the step invoked an MCP tool, including one it
	// refused.
	ToolCall *ToolCall

	// Evidence is set on a finish step, already resolved to step and tool
	// call ids.
	Evidence []Evidence
}

// StepRef is what the callback wrote.
//
// The callback returns it so the loop can resolve finish's citations itself.
// That keeps the whole of evidence validation in this package, and means a
// test's fake callback only has to return counterfeit ids.
type StepRef struct {
	StepID     string
	ToolCallID string
}

// ReportStep persists a step, publishes its event, and returns the ids it
// wrote.
//
// An error from it ends the run FAILED: a step that could not be persisted
// means the audit trail is already wrong, and continuing would make it worse.
type ReportStep func(context.Context, Step) (StepRef, error)

// ToolServer is the operational tool boundary. *mcpclient.Client satisfies
// it; the interface is here so the loop's tests need no MCP server.
type ToolServer interface {
	Tools() []mcpclient.Tool
	Call(ctx context.Context, name string, args json.RawMessage) (mcpclient.Result, error)
}

// ChunkSearcher is the retrieval half of search_knowledge. *search.Client
// satisfies it.
type ChunkSearcher interface {
	Search(ctx context.Context, vector []float32, q search.Query) ([]search.Result, error)
}
