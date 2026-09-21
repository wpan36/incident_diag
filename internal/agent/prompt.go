package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SystemPrompt is the standing instruction for every run.
//
// It says four things, and each of them is load-bearing somewhere else in
// this package: one tool per step, because agent_steps records one action per
// step; read-only, because every tool behind ops-mcp is; claims must rest on
// observations, because the evidence table is how that is checked afterwards;
// and finish before the budget runs out, because a run that does not is cut
// off by a forced finish that has less to work with.
const SystemPrompt = `You are a site reliability engineer investigating one production incident.
You are read-only: you can observe the system, but you cannot change it, and no tool you have will.

How you work:

- Each step you take calls exactly one of the tools you have been given. Never answer in prose; if you have nothing else to do, call finish.
- Every observation you are given is labelled with the number of the step that produced it. Those are the numbers finish's evidence cites.
- Start from what the incident says. Use search_knowledge to find the runbooks, postmortems and service documents this organisation has already written about these symptoms; several documents describe similar symptoms with different causes, so read enough to tell them apart.
- Use the operational tools to check what is actually happening now — metrics, logs, and the health of a service — rather than assuming the runbook's example is this incident.
- A tool that refuses your arguments is telling you how to call it correctly. A tool that fails is a dependency that is down; investigate around it rather than retrying it.

How you finish:

- Call finish when you can name a root cause, and call it before your budget of steps and tool calls runs out. A short investigation that ends in a diagnosis is worth more than a long one that is cut off.
- Every claim in your diagnosis must rest on something you actually observed in this run. Do not state a cause you did not check, and do not repeat a runbook's example as if you had measured it.
- Cite the steps your conclusion rests on in finish's evidence, using the step numbers the observations above are labelled with. For a step that searched the knowledge base, include the document_id of the specific document you are relying on, exactly as the search result printed it in square brackets.
- If the evidence does not support a confident answer, say so in root_cause and give the next actions that would settle it. An honest partial answer is a correct answer.`

// incidentMessage renders the incident as the run's first user message.
func incidentMessage(in Incident) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Incident %s\n", in.ID)
	fmt.Fprintf(&b, "Title: %s\n", in.Title)
	if in.Service != "" {
		fmt.Fprintf(&b, "Reported service: %s\n", in.Service)
	} else {
		b.WriteString("Reported service: not stated\n")
	}
	if !in.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "Filed at: %s\n", in.CreatedAt.UTC().Format(time.RFC3339))
	}
	b.WriteString("\nDescription:\n")
	if in.Description == "" {
		b.WriteString("(none given)")
	} else {
		b.WriteString(in.Description)
	}
	return b.String()
}

// observationMessage labels an observation with the step that produced it.
//
// finish cites steps by number and nothing else in the context says what those
// numbers are, so a model counting its own turns would be wrong from the first
// none step onwards. The label is added after the observation has been capped,
// so the context and the audit row still agree about the observation itself.
//
// An observation is never rendered empty: a tool message with no content is
// rejected by OpenAI-compatible providers, and that status is in the family
// this client does not retry, so one tool returning nothing would end the run.
func observationMessage(step int, observation string) string {
	if strings.TrimSpace(observation) == "" {
		observation = "(the tool returned no output)"
	}
	return fmt.Sprintf("Step %d observation:\n%s", step, observation)
}

// noToolMessage is what a step that called no usable tool renders as.
//
// A step with no tool call has no tool reply to pair an assistant message
// with, so it cannot be replayed the way the others are — and dropping it
// silently would leave the model no sign that it misbehaved, which is exactly
// what the retry exists to correct.
//
// It carries the reason, because "call a tool" is the wrong correction for two
// of the three things that get here: a finish whose arguments would not decode
// and a finish whose root_cause was blank both called a tool, and a retry that
// is not told what was wrong repeats it.
func noToolMessage(step int, reason string) string {
	if strings.TrimSpace(reason) == "" {
		reason = "it called no usable tool"
	}
	return fmt.Sprintf("Step %d was not usable: %s. That step number is now spent. "+
		"Every step must call exactly one of the tools you were given, with arguments "+
		"that match its schema. Call one now.", step, reason)
}

// forcedFinishMessage explains why the model is being cut off.
//
// Without it the last turn arrives with only one tool available and no reason
// given, and the model has to guess whether its investigation succeeded or was
// interrupted — which changes what it writes in root_cause.
func forcedFinishMessage(reason string) string {
	return fmt.Sprintf("This investigation has reached its limit (%s), so it must end now. "+
		"Call finish with the best diagnosis the observations above support. "+
		"If they do not support a confident one, say so in root_cause and put what "+
		"remains to be checked in next_actions.", reason)
}

// searchKnowledgeTool is the definition handed to the model.
//
// k's default is 5 rather than search.DefaultK: the observation cap is 8 KiB,
// roughly 2000 tokens, and a chunk runs to about 400, so ten hits do not fit
// and five do.
var searchKnowledgeTool = llmToolDef(ToolSearchKnowledge,
	"Search this organisation's runbooks, postmortems and service documents for passages "+
		"relevant to a question. Returns the most similar passages, each preceded by its "+
		"document id in square brackets.",
	`{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "What to look for, in the words a runbook would use — symptoms, metric names, error messages."
    },
    "service": {
      "type": "string",
      "description": "Restrict the search to documents about one service, such as payment-service."
    },
    "document_type": {
      "type": "string",
      "enum": ["runbook", "postmortem", "service_doc"],
      "description": "Restrict the search to one kind of document."
    },
    "k": {
      "type": "integer",
      "minimum": 1,
      "maximum": 50,
      "description": "How many passages to return. Defaults to 5, which is what fits in one observation."
    }
  },
  "required": ["query"],
  "additionalProperties": false
}`)

// finishTool ends the run.
//
// All four parameters are required. evidence may be an empty array — a run cut
// short by a bound may honestly have nothing worth citing — but the field
// itself is not optional, because required is the only mechanism behind the
// system prompt's demand that every claim rest on an observation.
var finishTool = llmToolDef(ToolFinish,
	"End the investigation with your diagnosis. Call this once you can name a root cause, "+
		"and call it before your budget of steps and tool calls runs out.",
	`{
  "type": "object",
  "properties": {
    "root_cause": {
      "type": "string",
      "description": "What is actually wrong, in one or two sentences, based only on what you observed in this run."
    },
    "affected_service": {
      "type": "string",
      "description": "The service the root cause is in, which is not always the service the incident was filed against."
    },
    "next_actions": {
      "type": "array",
      "items": {"type": "string"},
      "description": "What a responder should do next, most useful first."
    },
    "evidence": {
      "type": "array",
      "description": "The steps this diagnosis rests on. May be empty only if you observed nothing usable.",
      "items": {
        "type": "object",
        "properties": {
          "step": {"type": "integer", "description": "The number of the step whose observation supports this."},
          "note": {"type": "string", "description": "Why that observation supports the diagnosis."},
          "document_id": {
            "type": "string",
            "description": "For a step that searched the knowledge base, the id in square brackets of the document you relied on."
          }
        },
        "required": ["step", "note"],
        "additionalProperties": false
      }
    }
  },
  "required": ["root_cause", "affected_service", "next_actions", "evidence"],
  "additionalProperties": false
}`)

// llmToolDef panics on a schema that is not JSON, which is a programmer error
// in a constant in this file and can never be anything else.
func llmToolDef(name, description, schema string) toolDefinition {
	if !json.Valid([]byte(schema)) {
		panic("agent: the JSON schema for " + name + " is not valid JSON")
	}
	return toolDefinition{name: name, description: description, schema: json.RawMessage(schema)}
}

type toolDefinition struct {
	name        string
	description string
	schema      json.RawMessage
}
