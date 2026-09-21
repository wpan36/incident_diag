package agent

import (
	"encoding/json"
	"fmt"

	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/store"
	"github.com/wpan36/incident_diag/internal/summary"
)

// decodeFinal reads finish's arguments, tolerating a citation it cannot read.
//
// json.Unmarshal is all or nothing, so one citation whose fields have the
// wrong types would discard the diagnosis it belongs to — which is exactly
// what resolveEvidence already refuses to do for a citation naming a step that
// never happened. The fields a model gets wrong are therefore decoded on their
// own and dropped with a warning, and only arguments with no readable
// root_cause make the step a none.
//
// The residual case is a single citation whose step is a string rather than a
// number: that citation is dropped, not the diagnosis.
func (a *Agent) decodeFinal(runID string, args json.RawMessage) (FinalResult, error) {
	var raw struct {
		RootCause       string          `json:"root_cause"`
		AffectedService string          `json:"affected_service"`
		NextActions     json.RawMessage `json:"next_actions"`
		Evidence        json.RawMessage `json:"evidence"`
	}
	if err := json.Unmarshal(args, &raw); err != nil {
		return FinalResult{}, err
	}
	return FinalResult{
		RootCause:       raw.RootCause,
		AffectedService: raw.AffectedService,
		NextActions:     a.decodeNextActions(runID, raw.NextActions),
		Evidence:        a.decodeCitations(runID, raw.Evidence),
	}, nil
}

// decodeNextActions accepts the bare string a model sometimes sends where the
// schema asks for an array of them.
func (a *Agent) decodeNextActions(runID string, raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var actions []string
	if err := json.Unmarshal(raw, &actions); err == nil {
		return actions
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	a.deps.Logger.Warn("dropping next_actions that could not be read",
		"run_id", runID, "next_actions", string(raw))
	return nil
}

// decodeCitations reads the evidence array one item at a time.
func (a *Agent) decodeCitations(runID string, raw json.RawMessage) []Citation {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		a.deps.Logger.Warn("dropping an evidence array that could not be read",
			"run_id", runID, "evidence", string(raw), "error", err)
		return nil
	}
	out := make([]Citation, 0, len(items))
	for _, item := range items {
		var c Citation
		if err := json.Unmarshal(item, &c); err != nil {
			a.deps.Logger.Warn("dropping a citation that could not be read",
				"run_id", runID, "citation", string(item), "error", err)
			continue
		}
		out = append(out, c)
	}
	return out
}

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
