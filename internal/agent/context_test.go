package agent

import (
	"strings"
	"testing"

	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/summary"
)

func testBuilder() ContextBuilder {
	return ContextBuilder{
		Incident: Incident{
			ID: "inc-1", Title: "checkout is slow", Description: "p99 above 3s",
			Service: "checkout-service",
		},
		Budget: Budget{MaxSteps: 8, MaxToolCalls: 6},
	}
}

// testSpent is a mid-run position, far enough from either bound that the
// budget line says nothing urgent.
func testSpent() Spent { return Spent{Step: 2, ToolCalls: 1} }

func TestBuildStartsWithThePromptAndTheIncident(t *testing.T) {
	msgs := testBuilder().Build(nil, testSpent())

	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3", len(msgs))
	}
	if msgs[0].Role != llm.RoleSystem || msgs[0].Content != SystemPrompt {
		t.Errorf("first message = %+v", msgs[0])
	}
	if msgs[1].Role != llm.RoleUser {
		t.Errorf("second message role = %q", msgs[1].Role)
	}
	for _, want := range []string{"inc-1", "checkout is slow", "checkout-service", "p99 above 3s"} {
		assertContains(t, msgs[1].Content, want, "the incident message")
	}
}

// The model has to be able to see the wall it is walking into. Before this
// line existed the prompt told it to "finish before your budget runs out" and
// never said what the budget was — and across twenty-four evaluation runs, not
// one called finish on its own.
func TestBuildTellsTheModelItsBudget(t *testing.T) {
	msgs := testBuilder().Build(nil, Spent{Step: 3, ToolCalls: 2})

	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("the budget line is not a user message: %+v", last)
	}
	for _, want := range []string{"step 3 of 8", "2 of 6 tool calls"} {
		assertContains(t, last.Content, want, "the budget line")
	}
	// The accounting rule, because a model cannot infer it: search_knowledge
	// writes no tool_calls row, so it does not count toward MaxToolCalls.
	assertContains(t, last.Content, "does not count", "the budget line")
}

// The pressure has to build rather than arrive all at once: the loop checks
// its bounds before the model speaks, so a run with no warning goes from
// "nothing said" to "cut off" in one turn.
func TestBudgetLineSharpensNearTheBound(t *testing.T) {
	for _, tc := range []struct {
		name string
		step int
		want string
	}{
		{"mid-run", 3, "5 steps remain after this one"},
		{"one left", 7, "One step remains"},
		{"the last one", 8, "last step"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msgs := testBuilder().Build(nil, Spent{Step: tc.step})
			assertContains(t, msgs[len(msgs)-1].Content, tc.want, "the budget line")
		})
	}
}

// A tool call sent in history without its paired reply is rejected outright by
// some OpenAI-compatible providers, and M33 exists to switch providers.
func TestBuildPairsEveryToolCallWithAReply(t *testing.T) {
	turns := []Turn{
		{Number: 1, Call: llm.NewToolCall("c1", ToolSearchKnowledge, map[string]any{"query": "pool"}), Observation: "a runbook"},
		{Number: 2, None: true},
		{Number: 3, Call: llm.NewToolCall("c2", "prometheus_query", map[string]any{"query": "up"}), Observation: "1"},
	}

	msgs := testBuilder().Build(turns, testSpent())

	var pending string
	for _, m := range msgs {
		switch {
		case len(m.ToolCalls) > 0:
			if pending != "" {
				t.Fatalf("tool call %s has no reply before %s", pending, m.ToolCalls[0].ID)
			}
			if m.Role != llm.RoleAssistant || m.Content != "" {
				t.Errorf("assistant message = %+v, want no content", m)
			}
			pending = m.ToolCalls[0].ID
		case m.Role == llm.RoleTool:
			if m.ToolCallID != pending {
				t.Errorf("reply %q does not pair with %q", m.ToolCallID, pending)
			}
			pending = ""
		}
	}
	if pending != "" {
		t.Errorf("tool call %s was never replied to", pending)
	}
}

// A step with no tool call has no reply to pair with, and dropping it silently
// would leave the model no sign that it misbehaved.
func TestBuildRendersANoneStepAsAnInstruction(t *testing.T) {
	msgs := testBuilder().Build([]Turn{
		{Number: 2, None: true, Reason: "finish was called with an empty root_cause"},
	}, testSpent())

	// -2, not -1: the budget line is always the last message now.
	last := msgs[len(msgs)-2]
	if last.Role != llm.RoleUser || len(last.ToolCalls) > 0 {
		t.Fatalf("the none instruction = %+v", last)
	}
	assertContains(t, last.Content, "exactly one of the tools", "the none instruction")
	// The reason, not just "call a tool": two of the three responses that get
	// here did call one, and a retry not told what was wrong repeats it.
	assertContains(t, last.Content, "empty root_cause", "the none instruction")
}

// The context and the audit row are capped by the same helper at the same
// limit, so they cannot disagree about what was said. The step label is added
// after the cap, so it costs the observation nothing.
func TestBuildCapsAnOversizedObservation(t *testing.T) {
	huge := strings.Repeat("x", summary.LimitBytes+500)
	msgs := testBuilder().Build([]Turn{
		{Number: 1, Call: llm.NewToolCall("c1", "prometheus_query", nil), Observation: huge},
	}, testSpent())

	reply := msgs[len(msgs)-2]
	_, observation, found := strings.Cut(reply.Content, "\n")
	if !found {
		t.Fatalf("the reply carries no step label: %.60q", reply.Content)
	}
	if len(observation) != summary.LimitBytes {
		t.Errorf("the observation is %d bytes, want the %d-byte cap", len(observation), summary.LimitBytes)
	}
}

// finish cites steps by number, and the context is the only place the model
// can learn what those numbers were: a none step spends one without leaving an
// assistant turn to count, so a model counting its own turns would be wrong
// from that point on.
func TestBuildLabelsEveryObservationWithItsStepNumber(t *testing.T) {
	msgs := testBuilder().Build([]Turn{
		{Number: 1, Call: llm.NewToolCall("c1", ToolSearchKnowledge, nil), Observation: "a runbook"},
		{Number: 2, None: true, Reason: "the response called no tool"},
		{Number: 3, Call: llm.NewToolCall("c2", "prometheus_query", nil), Observation: "1"},
	}, testSpent())

	assertContains(t, msgs[3].Content, "Step 1 observation:", "the first reply")
	assertContains(t, msgs[4].Content, "Step 2 was not usable", "the none instruction")
	assertContains(t, msgs[6].Content, "Step 3 observation:", "the second reply")
	// The step a none spent is not reused, so the numbers the model sees are
	// the numbers the rows were written under.
	if strings.Contains(msgs[6].Content, "Step 2") {
		t.Errorf("the step after a none was renumbered: %q", msgs[6].Content)
	}
}

// A tool message with no content is rejected by OpenAI-compatible providers,
// and that status is not retried — one tool returning nothing would end the
// run.
func TestBuildNeverSendsAnEmptyToolReply(t *testing.T) {
	msgs := testBuilder().Build([]Turn{
		{Number: 1, Call: llm.NewToolCall("c1", "http_probe", nil), Observation: ""},
	}, testSpent())

	reply := msgs[len(msgs)-2]
	if reply.Role != llm.RoleTool {
		t.Fatalf("last message = %+v", reply)
	}
	if strings.TrimSpace(reply.Content) == "" {
		t.Error("the tool reply has no content")
	}
	assertContains(t, reply.Content, "no output", "the empty observation's placeholder")
}

func TestEstimateTokensCountsEveryPartOfTheMessages(t *testing.T) {
	msgs := []llm.Message{{Role: "user", Content: strings.Repeat("a", 400)}}

	// 400 bytes of content plus the four-byte role, at four bytes per token.
	if got := EstimateTokens(msgs); got != 101 {
		t.Errorf("EstimateTokens = %d, want 101", got)
	}
	if EstimateTokens(nil) != 0 {
		t.Error("an empty prompt should estimate as zero tokens")
	}
}
