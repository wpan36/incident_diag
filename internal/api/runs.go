package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/store"
	"github.com/wpan36/incident_diag/internal/wire"
)

// createRun starts an investigation of one incident.
//
// The request body is empty and the budget is not overridable. config.LoadAgent
// ties an invariant to configuration loading — MaxSteps × the 8 KiB
// observation cap must fit the token ceiling, which is what makes "the context
// is never pruned" true — and a request that set its own max_steps would give
// that invariant a second enforcement point, where a violating run would break
// the no-pruning guarantee silently.
//
// The 409 comes from uniq_active_run refusing the insert, not from a
// read-then-write check that two requests could interleave. The 404 comes from
// reading the incident first: a run against a missing incident violates
// fk_agent_runs_incident, and dbError does not classify a foreign-key failure
// as not-found, so without the read the client would get a 500 for its own
// mistake.
func (s *Server) createRun(c *gin.Context) {
	incidentID := c.Param("id")
	if !validID(incidentID) {
		renderError(c, httpx.NotFound("incident %s not found", incidentID))
		return
	}
	if _, err := s.deps.Store.GetIncident(c.Request.Context(), incidentID); err != nil {
		renderError(c, err)
		return
	}

	// The budget and the model are recorded on the row, as S7 requires, so a
	// run stays interpretable after the configuration changes. The worker
	// takes the budget from the row; the model it cannot, since llm.Client
	// holds its own and Chat takes no override, so this column records what
	// the API believed rather than instructing the worker.
	run, err := s.deps.Store.CreateRun(c.Request.Context(), store.NewRun{
		IncidentID:         incidentID,
		Model:              s.deps.LLMModel,
		MaxSteps:           s.deps.Agent.MaxSteps,
		MaxToolCalls:       s.deps.Agent.MaxToolCalls,
		MaxDurationSeconds: int(s.deps.Agent.MaxRunDuration.Seconds()),
		MaxPromptTokens:    s.deps.Agent.MaxPromptTokens,
	})
	if err != nil {
		renderError(c, err)
		return
	}

	s.enqueueRun(c, run.ID)
	renderCreated(c, "/api/runs/"+run.ID, wire.NewRun(run))
}

// enqueueRun asks a worker to execute the run that was just created.
//
// A produce that fails is logged and nothing else, and the response is still
// 201 — the same trade POST /api/documents makes and for the same reason. The
// run genuinely was created, so 500 would be a lie, and a client retrying on
// it would hit 409 from its own first attempt. The row is PENDING, which is
// what the reconciler's never-enqueued category sweeps for.
func (s *Server) enqueueRun(c *gin.Context, runID string) {
	ctx := c.Request.Context()
	if err := s.deps.Producer.Produce(ctx, mq.TopicAgentRuns, runID, mq.NewRunMessage(runID)); err != nil {
		s.deps.Logger.ErrorContext(ctx, "could not enqueue run for execution",
			"run_id", runID, "error", err)
	}
}

// getRun returns a run with its whole timeline: every step, each step's tool
// call, and the evidence the run kept.
//
// It is not paginated. max_steps bounds the step count at single digits, and a
// timeline is read whole or not at all. This is the one place the API has two
// shapes for one entity — wire.Run for a listing, wire.RunDetail here — and
// the alternative, a listing carrying every run's whole timeline, is worse.
func (s *Server) getRun(c *gin.Context) {
	runID := c.Param("id")
	if !validID(runID) {
		renderError(c, httpx.NotFound("run %s not found", runID))
		return
	}

	ctx := c.Request.Context()
	run, err := s.deps.Store.GetRun(ctx, runID)
	if err != nil {
		renderError(c, err)
		return
	}
	steps, err := s.deps.Store.ListStepsByRun(ctx, runID)
	if err != nil {
		renderError(c, err)
		return
	}
	toolCalls, err := s.deps.Store.ListToolCallsByRun(ctx, runID)
	if err != nil {
		renderError(c, err)
		return
	}
	evidence, err := s.deps.Store.ListEvidenceByRun(ctx, runID)
	if err != nil {
		renderError(c, err)
		return
	}
	c.JSON(http.StatusOK, wire.NewRunDetail(run, steps, toolCalls, evidence))
}

// listIncidentRuns returns one page of an incident's runs, newest first.
//
// It reads the incident first for a smaller reason than createRun does: an
// empty page and a mistyped id must not look the same.
func (s *Server) listIncidentRuns(c *gin.Context) {
	incidentID := c.Param("id")
	if !validID(incidentID) {
		renderError(c, httpx.NotFound("incident %s not found", incidentID))
		return
	}

	var v validation
	p := parsePageParams(c, &v)
	if err := v.err(); err != nil {
		renderError(c, err)
		return
	}

	ctx := c.Request.Context()
	if _, err := s.deps.Store.GetIncident(ctx, incidentID); err != nil {
		renderError(c, err)
		return
	}

	page, err := s.deps.Store.ListRunsByIncident(ctx, incidentID, p)
	if err != nil {
		renderError(c, err)
		return
	}
	c.JSON(http.StatusOK, newRunList(page))
}

// newRunList converts a page of stored runs, always producing a non-nil slice
// so the JSON is [] rather than null.
func newRunList(page store.Page[store.Run]) list[wire.Run] {
	items := make([]wire.Run, 0, len(page.Items))
	for _, r := range page.Items {
		items = append(items, wire.NewRun(r))
	}
	return list[wire.Run]{Items: items, NextCursor: page.NextCursor}
}
