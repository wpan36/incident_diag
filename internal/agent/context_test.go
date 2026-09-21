package agent

import (
	"strings"
	"testing"

	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/summary"
)

func testBuilder() ContextBuilder {
	return ContextBuilder{Incident: Incident{
		ID: "inc-1", Title: "checkout is slow", Description: "p99 above 3s",
		Service: "checkout-service",
	}}
}

func TestBuildStartsWithThePromptAndTheIncident(t *testing.T) {
	msgs := testBuilder().Build(nil)

	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
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

// A tool call sent in history without its paired reply is rejected outright by
// some OpenAI-compatible providers, and M33 exists to switch providers.
func TestBuildPairsEveryToolCallWithAReply(t *testing.T) {
	turns := []Turn{
		{Call: llm.NewToolCall("c1", ToolSearchKnowledge, map[string]any{"query": "pool"}), Observation: "a runbook"},
		{None: true},
		{Call: llm.NewToolCall("c2", "prometheus_query", map[string]any{"query": "up"}), Observation: "1"},
	}

	msgs := testBuilder().Build(turns)

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
	msgs := testBuilder().Build([]Turn{{None: true}})

	last := msgs[len(msgs)-1]
	if last.Role != llm.RoleUser || len(last.ToolCalls) > 0 {
		t.Fatalf("last message = %+v", last)
	}
	assertContains(t, last.Content, "exactly one of the tools", "the none instruction")
}

// The context and the audit row are capped by the same helper at the same
// limit, so they cannot disagree about what was said.
func TestBuildCapsAnOversizedObservation(t *testing.T) {
	huge := strings.Repeat("x", summary.LimitBytes+500)
	msgs := testBuilder().Build([]Turn{
		{Call: llm.NewToolCall("c1", "prometheus_query", nil), Observation: huge},
	})

	reply := msgs[len(msgs)-1]
	if len(reply.Content) != summary.LimitBytes {
		t.Errorf("the reply is %d bytes, want the %d-byte cap", len(reply.Content), summary.LimitBytes)
	}
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
