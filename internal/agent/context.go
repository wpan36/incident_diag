package agent

import (
	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/summary"
)

// Turn is one completed step, as the context replays it.
//
// The assistant message carries the executed tool call and nothing else: the
// model's prose reasoning is never replayed, because the schema has nowhere
// to persist it and a context that contained it would disagree with the audit
// trail about what happened.
type Turn struct {
	// Number is the step number the loop recorded this turn under, and it is
	// what labels the observation in the context.
	//
	// Without it the model has no way to learn the numbers finish's evidence
	// cites: a none step spends a step number without leaving an assistant
	// turn to count, so a model counting its own turns would cite the step
	// before the one it meant.
	Number int

	// Call is the tool call that was executed. When several were returned,
	// only this one appears — an assistant message whose other tool calls have
	// no paired reply is exactly the history some providers reject.
	Call llm.ToolCall

	// Observation is the full text; Build caps it.
	Observation string

	// None marks a step whose response called no usable tool. It has no tool
	// reply to pair with, so it renders as an instruction instead.
	None bool

	// Reason is why that response was unusable — no tool call, arguments that
	// will not decode, a blank root_cause. It is rendered into the
	// instruction, because a retry told only "call a tool" repeats whichever
	// of those three it did.
	Reason string
}

// ContextBuilder assembles the prompt. It only assembles — there is nothing
// to prune.
//
// MaxSteps multiplied by the 8 KiB observation cap is the ceiling on the
// context, and config.LoadAgent refuses a configuration where that ceiling
// does not fit inside MaxPromptTokens. A sliding window or a summarizer would
// be a strategy, and a test suite, for a situation that cannot arise.
type ContextBuilder struct {
	Incident Incident
}

// Build renders the message sequence for the next call.
func (b ContextBuilder) Build(turns []Turn) []llm.Message {
	msgs := make([]llm.Message, 0, 2+2*len(turns))
	msgs = append(msgs,
		llm.Message{Role: llm.RoleSystem, Content: SystemPrompt},
		llm.Message{Role: llm.RoleUser, Content: incidentMessage(b.Incident)},
	)

	for _, t := range turns {
		if t.None {
			msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: noToolMessage(t.Number, t.Reason)})
			continue
		}
		// Capped with the same helper and the same limit internal/store
		// applies when persisting, so the context and the audit row cannot
		// disagree about what was said. The step label is added after the cap,
		// so it costs the observation nothing.
		observation, _, _ := summary.Cap(t.Observation)
		msgs = append(msgs,
			llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{t.Call}},
			llm.Message{Role: llm.RoleTool, ToolCallID: t.Call.ID,
				Content: observationMessage(t.Number, observation)},
		)
	}
	return msgs
}

// EstimateTokens approximates how large a prompt is, at the worst-case ratio
// config.LoadAgent validates its invariant with.
//
// The two have to use the same number or the bound could fire on a
// configuration that had just been accepted. It counts the messages rather
// than the tool definitions, which is what config's 40 KiB of overhead
// covers alongside the incident and the system prompt.
func EstimateTokens(msgs []llm.Message) int {
	bytes := 0
	for _, m := range msgs {
		bytes += len(m.Role) + len(m.Content) + len(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			bytes += len(tc.ID) + len(tc.Type) + len(tc.Function.Name) + len(tc.Function.Arguments)
		}
	}
	return (bytes + config.BytesPerToken - 1) / config.BytesPerToken
}
