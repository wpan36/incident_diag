package agent

import (
	"fmt"

	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/store"
	"github.com/wpan36/incident_diag/internal/summary"
)

// resolveEvidence turns finish's citations into rows.
//
// A step number or a document_id the model invented is dropped with a warning
// rather than failing the run: a wrong citation should not discard a correct
// diagnosis. An invented step drops the row; an invented document leaves
// document_id empty and keeps it.
func (a *Agent) resolveEvidence(s *runState, citations []Citation) []Evidence {
	out := make([]Evidence, 0, len(citations))
	for _, c := range citations {
		rec, known := s.records[c.Step]
		if !known {
			a.deps.Logger.Warn("dropping a citation of a step that did not happen",
				"run_id", s.run.ID, "cited_step", c.Step)
			continue
		}

		e := Evidence{StepID: rec.ref.StepID, Note: c.Note}
		switch rec.actionType {
		case store.ActionRetrieve:
			e.SourceType = store.SourceRetrieval
			if hit, found := findHit(rec.hits, c.DocumentID); found {
				e.DocumentID = hit.DocumentID
				e.SourceRef = hit.Source
				e.Summary = hit.Content
			} else {
				if c.DocumentID != "" {
					a.deps.Logger.Warn("dropping a cited document that the step did not return",
						"run_id", s.run.ID, "cited_step", c.Step, "document_id", c.DocumentID)
				}
				e.SourceRef = fmt.Sprintf("%s(%s)", ToolSearchKnowledge, rec.query)
				e.Summary = rec.observation
			}

		case store.ActionToolCall:
			e.SourceType = store.SourceTool
			e.ToolCallID = rec.ref.ToolCallID
			e.SourceRef = rec.toolName
			e.Summary = rec.observation

		default:
			// A none step, or the finish step itself. source_type has two
			// values and neither describes a step that did nothing.
			a.deps.Logger.Warn("dropping a citation of a step that produced no observation",
				"run_id", s.run.ID, "cited_step", c.Step, "action_type", rec.actionType)
			continue
		}

		// The same helper the store applies, so the row and the context agree
		// about what was cited.
		e.Summary, _, _ = summary.Cap(e.Summary)
		out = append(out, e)
	}
	return out
}

// findHit locates the cited document among a retrieval's hits. An empty id is
// not a match: the model did not name a document, so there is nothing to
// validate and nothing to record.
func findHit(hits []search.Result, documentID string) (search.Result, bool) {
	if documentID == "" {
		return search.Result{}, false
	}
	for _, h := range hits {
		if h.DocumentID == documentID {
			return h, true
		}
	}
	return search.Result{}, false
}
