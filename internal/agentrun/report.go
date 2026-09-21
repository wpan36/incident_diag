package agentrun

import (
	"context"
	"fmt"

	"github.com/wpan36/incident_diag/internal/agent"
	"github.com/wpan36/incident_diag/internal/events"
	"github.com/wpan36/incident_diag/internal/store"
	"github.com/wpan36/incident_diag/internal/wire"
)

// ReportStep is the agent.ReportStep the loop is built with: it writes a
// step's rows and then publishes step.completed.
//
// The three writes happen in that order — the step, its tool_calls row, its
// evidence rows — because the foreign keys require it, and not in one
// transaction. A crash between two of them leaves rows that the restart
// deletes anyway, so a transaction would buy atomicity nothing reads.
//
// An error from here ends the run FAILED, which is why only a failed *write*
// returns one. A failed publish does not: the audit trail is correct and only
// the live timeline is missing an entry.
//
// The event is published after the rows exist, so a client that reads
// GET /api/runs/{id} and then subscribes can never see an event for something
// unpersisted. The cost, stated rather than left to be discovered: the browser
// shows nothing while a step runs — an LLM call plus a tool call, roughly four
// seconds — because there is no step.started. M28 will have a UI to judge
// that with, and adding one then is additive.
func (h *Handler) ReportStep(ctx context.Context, step agent.Step) (agent.StepRef, error) {
	written, err := h.deps.Store.CreateStep(ctx, store.NewStep{
		RunID:      step.RunID,
		StepNumber: step.StepNumber,
		ActionType: step.ActionType,
		Action:     step.Action,
		// The full text. CreateStep caps it and records the original length
		// and the flag together, so those three cannot disagree.
		Observation: step.Observation,
		Status:      step.Status,
		Error:       step.Error,
		Duration:    step.Duration,
	})
	if err != nil {
		return agent.StepRef{}, fmt.Errorf("write step %d: %w", step.StepNumber, err)
	}
	ref := agent.StepRef{StepID: written.ID}

	var toolCall *store.ToolCall
	if step.ToolCall != nil {
		tc, err := h.deps.Store.CreateToolCall(ctx, store.NewToolCall{
			RunID:     step.RunID,
			StepID:    written.ID,
			ToolName:  step.ToolCall.Name,
			Arguments: step.ToolCall.Arguments,
			Status:    step.ToolCall.Status,
			Result:    step.ToolCall.Result,
			Error:     step.ToolCall.Error,
			Duration:  step.ToolCall.Duration,
		})
		if err != nil {
			return agent.StepRef{}, fmt.Errorf("write the tool call of step %d: %w", step.StepNumber, err)
		}
		toolCall = &tc
		ref.ToolCallID = tc.ID
	}

	// Evidence is set on a finish step only, and the loop has already resolved
	// each citation to the step and tool call it names — including ones from
	// earlier steps, which is why StepID comes from the evidence rather than
	// from the step being written.
	live, err := h.liveDocuments(ctx, step.Evidence)
	if err != nil {
		return agent.StepRef{}, fmt.Errorf("check the evidence of step %d: %w", step.StepNumber, err)
	}

	evidence := make([]store.Evidence, 0, len(step.Evidence))
	for _, e := range step.Evidence {
		documentID := e.DocumentID
		if documentID != "" && !live[documentID] {
			// The chunk is in the index and its document row is not, which
			// fk_evidence_document would refuse. The row is still written:
			// source_ref is what keeps a citation readable once the document
			// it points at has gone, which is the same reason deleting a
			// document nulls this column rather than removing the row. It
			// matches what the loop already does with an invented document —
			// a wrong citation should not discard a correct diagnosis.
			h.deps.Logger.WarnContext(ctx, "dropping a citation to a document that no longer exists",
				"step", step.StepNumber, "document_id", documentID, "source_ref", e.SourceRef)
			documentID = ""
		}

		row, err := h.deps.Store.CreateEvidence(ctx, store.NewEvidence{
			RunID:      step.RunID,
			StepID:     e.StepID,
			ToolCallID: e.ToolCallID,
			SourceType: e.SourceType,
			SourceRef:  e.SourceRef,
			DocumentID: documentID,
			Summary:    e.Summary,
			Note:       e.Note,
		})
		if err != nil {
			return agent.StepRef{}, fmt.Errorf("write the evidence of step %d: %w", step.StepNumber, err)
		}
		evidence = append(evidence, row)
	}

	h.publish(ctx, step.RunID, events.StepCompleted, wire.NewStep(written, toolCall, evidence))
	return ref, nil
}

// liveDocuments returns which of the documents this step's evidence cites are
// still in the documents table.
//
// The loop validates a cited document_id against the hits the retrieval
// returned, and those come from Elasticsearch — so an id can be real and
// current there while its MySQL row has since gone. One query per finish
// step, and none at all for every other step.
func (h *Handler) liveDocuments(ctx context.Context, evidence []agent.Evidence) (map[string]bool, error) {
	var ids []string
	seen := map[string]bool{}
	for _, e := range evidence {
		if e.DocumentID != "" && !seen[e.DocumentID] {
			seen[e.DocumentID] = true
			ids = append(ids, e.DocumentID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return h.deps.Store.ExistingDocuments(ctx, ids)
}
