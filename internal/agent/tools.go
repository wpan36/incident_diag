package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/wpan36/incident_diag/internal/llm"
	"github.com/wpan36/incident_diag/internal/mcpclient"
	"github.com/wpan36/incident_diag/internal/obs"
	"github.com/wpan36/incident_diag/internal/store"
)

// metricUnknownTool stands in for a tool name the model invented, so that
// agent_tool_calls_total keeps a closed label set.
const metricUnknownTool = "__unknown__"

// Deps is everything the loop needs.
//
// A struct rather than positional parameters, following ingest.Deps: several
// of these are interfaces over the same shape and a caller that swaps two of
// them would still compile.
type Deps struct {
	LLM       llm.Chatter
	Knowledge *Knowledge
	Tools     ToolServer

	// Report persists a step and publishes its event. It is the only way this
	// package reaches a database or a broker.
	Report ReportStep

	Logger *slog.Logger
}

// Agent runs bounded investigations.
type Agent struct {
	deps Deps

	// definitions is the one list handed to the model: search_knowledge, the
	// tools discovered from ops-mcp, and finish. The model calls exactly one
	// per step.
	definitions []llm.Tool

	// mcpNames is the set of names that go to the tool server, so an
	// invented name can be told from a real one without asking the server.
	mcpNames map[string]struct{}

	// names is every valid name, listed in a refusal so the model can
	// recover.
	names []string
}

// New assembles the tool list once, at construction.
//
// ops-mcp lists its tools at connection time and never changes them, so
// building this per run would be work per run to learn nothing.
func New(deps Deps) *Agent {
	a := &Agent{deps: deps, mcpNames: map[string]struct{}{}}

	a.definitions = append(a.definitions, toolFor(searchKnowledgeTool))
	if deps.Tools != nil {
		for _, t := range deps.Tools.Tools() {
			if t.Name == ToolSearchKnowledge || t.Name == ToolFinish {
				// The loop dispatches by name, so a server tool sharing one of
				// these names would be unreachable anyway; dropping it loudly
				// beats a tool that is listed and never called.
				deps.Logger.Warn("ignoring a tool whose name the agent reserves", "tool", t.Name)
				continue
			}
			a.mcpNames[t.Name] = struct{}{}
			a.definitions = append(a.definitions, llm.Tool{
				Name: t.Name, Description: t.Description, InputSchema: t.InputSchema,
			})
		}
	}
	a.definitions = append(a.definitions, toolFor(finishTool))

	for _, d := range a.definitions {
		a.names = append(a.names, d.Name)
	}
	sort.Strings(a.names)
	return a
}

func toolFor(d toolDefinition) llm.Tool {
	return llm.Tool{Name: d.name, Description: d.description, InputSchema: d.schema}
}

// callTool invokes one MCP tool and turns its result into an observation.
//
// mcpclient.Result carries two independent signals and conflating them is the
// mistake this function exists to prevent. Refused means the tool ran and
// declined, which the model can fix by calling again with different
// arguments; a non-OK Status means the dependency did not answer, which it
// cannot. Both leave the step OK and the run going: a step's status says
// whether it produced a usable action and an observation, not whether the
// observation was good news.
func (a *Agent) callTool(ctx context.Context, name string, args json.RawMessage) Observation {
	if _, known := a.mcpNames[name]; !known {
		// Recorded like a refusal, which is how ops-mcp already answers an
		// unknown service name, so the model recovers the same way. It spends
		// one of MaxToolCalls: an agent that keeps inventing names must not
		// run unbounded.
		note := fmt.Sprintf("There is no tool called %q. The tools you may call are: %s.",
			name, strings.Join(a.names, ", "))
		// metricUnknownTool, not name: the model invents these, so the label
		// would be unbounded and one bad run could double the cardinality of
		// this metric for the life of the process.
		obs.AgentToolCalls.WithLabelValues(metricUnknownTool, store.ToolCallRefused).Inc()
		return Observation{
			Text:  note,
			Error: note,
			ToolCall: &ToolCall{
				Name: name, Arguments: args,
				Status: store.ToolCallRefused, Result: note, Error: note,
			},
		}
	}

	start := time.Now()
	res, err := a.deps.Tools.Call(ctx, name, args)
	elapsed := time.Since(start)
	// The name is known to be one of ops-mcp's, so the label is a closed set.
	obs.AgentToolDuration.WithLabelValues(name).Observe(elapsed.Seconds())

	if err != nil {
		obs.AgentToolCalls.WithLabelValues(name, store.ToolCallError).Inc()
		// The caller checks ctx.Err() before using this: a cancelled run stops
		// rather than recording a tool failure for a run that is over.
		text := fmt.Sprintf("%s could not be called: %v", name, err)
		return Observation{
			Text:  text,
			Error: text,
			ToolCall: &ToolCall{
				Name: name, Arguments: args,
				Status: store.ToolCallError, Result: text, Error: text, Duration: elapsed,
			},
		}
	}

	call := &ToolCall{
		Name: name, Arguments: args,
		Status: statusOf(res), Result: res.Text, Duration: elapsed,
	}
	obs.AgentToolCalls.WithLabelValues(name, call.Status).Inc()
	if call.Status != store.ToolCallOK {
		// The note is the server's one-line reason; the text is what the model
		// reads. Recording both means the audit row says why without anyone
		// having to read the whole result.
		call.Error = res.Note
		if call.Error == "" {
			call.Error = res.Text
		}
	}
	return Observation{Text: res.Text, ToolCall: call}
}

// statusOf maps a result onto tool_calls.status.
//
// Status wins over Refused when they disagree: mcpclient already reasons this
// way, so a result the server explicitly marked as an error can never be
// recorded as something the model merely got wrong.
func statusOf(res mcpclient.Result) string {
	switch res.Status {
	case mcpclient.StatusTimeout:
		return store.ToolCallTimeout
	case mcpclient.StatusError:
		return store.ToolCallError
	}
	if res.Refused {
		return store.ToolCallRefused
	}
	return store.ToolCallOK
}
