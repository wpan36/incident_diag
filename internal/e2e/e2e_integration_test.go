//go:build integration

// The product, end to end.
//
//	make up-lab && make test-integration
//
// It injects a fault into the Incident Lab, files an incident through the API,
// lets the agent investigate, and asserts on what came back. It needs the full
// stack, ops-mcp, and a key it is opted into with, because one run is one
// billed investigation.
//
// cmd/ingestion-worker and cmd/agent-worker must NOT be running. The harness
// joins their consumer groups, so a second member would take half the messages
// and fail them against a storage root it does not share.
//
// It does not assert on the wording of root_cause. A model is not
// deterministic and a test that pinned prose would fail for reasons that are
// not this project's.
package e2e

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/store"
)

// testTimeout covers the corpus upload, the warm-up and one investigation.
const testTimeout = 20 * time.Minute

func TestInvestigationNamesTheFaultedService(t *testing.T) {
	h, ctx := harness(t)

	s := DefaultScenarios()[0]
	if s.Name != "payment-latency" {
		t.Fatalf("the first default scenario is %s, not payment-latency", s.Name)
	}

	out, err := h.Run(ctx, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	report(t, out)

	// A bounded run that produced a diagnosis is a success, whatever stopped
	// it (S1), so the stop reason is reported and not asserted.
	if out.Status != store.RunSucceeded {
		t.Fatalf("status = %s (%s): %s", out.Status, out.StopReason, out.Error)
	}

	// An agent that answered from the model's own knowledge has not
	// investigated anything.
	if out.Retrievals() == 0 {
		t.Error("the run searched the knowledge base zero times")
	}
	if out.ToolCalls() == 0 {
		t.Error("the run called no tool")
	}

	// Contains rather than equals. The schema asks for a bare service name and
	// the field is still free text a model fills: one that answers
	// "payment-service (the card processor behind it)" has named the right
	// service, and this test does not pin prose.
	if !strings.Contains(strings.ToLower(out.AffectedService), strings.ToLower(s.ExpectService)) {
		t.Errorf("affected_service = %q, which does not name %q", out.AffectedService, s.ExpectService)
	}
	if strings.TrimSpace(out.RootCause) == "" {
		t.Error("the diagnosis has an empty root_cause")
	}

	// A diagnosis with no citations is a claim. The agent resolves each one to
	// the step that produced it, and the API hangs it off that step — so every
	// citation must name a step of this run.
	if out.EvidenceCount() == 0 {
		t.Error("the diagnosis cited nothing")
	}
	ids := map[string]bool{}
	for _, st := range out.Steps {
		ids[st.ID] = true
	}
	for _, st := range out.Steps {
		for _, ev := range st.Evidence {
			if ev.StepID != st.ID {
				t.Errorf("step %d carries evidence for step %s", st.Number, ev.StepID)
			}
			if !ids[ev.StepID] {
				t.Errorf("evidence cites step %s, which is not in this run", ev.StepID)
			}
		}
	}
}

// harness builds the system and cleans it up, or skips with the reason.
func harness(t *testing.T) (*Harness, context.Context) {
	t.Helper()

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("locating the repository root: %v", err)
	}

	cfg, err := ConfigFromEnv(t.TempDir(), filepath.Join(root, "testdata", "knowledge"), logger(t))
	if err != nil {
		t.Skipf("skipping the end-to-end run: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	t.Cleanup(cancel)

	h, err := New(ctx, cfg)
	if err != nil {
		t.Fatalf("standing the system up: %v", err)
	}
	t.Cleanup(h.Close)
	return h, ctx
}

// logger keeps the workers' logs out of the test output unless -v asked for
// them, because one investigation writes a few hundred lines.
func logger(t *testing.T) *slog.Logger {
	t.Helper()
	if testing.Verbose() {
		return log.New(os.Stderr, slog.LevelInfo, "e2e")
	}
	return log.Discard()
}

// report prints the whole trajectory, which is what says *why* a failing
// assertion failed.
func report(t *testing.T, out Outcome) {
	t.Helper()
	t.Logf("run %s: %s/%s in %s — steps=%d tool_calls=%d tokens=%d/%d",
		out.RunID, out.Status, out.StopReason, out.Elapsed.Round(time.Second),
		out.StepCount, out.ToolCallCount, out.PromptTokens, out.CompletionTokens)
	for _, s := range out.Steps {
		t.Logf("  step %d  %-10s %-8s %-18s %s",
			s.Number, s.ActionType, s.Status, s.Tool, s.Error)
	}
	t.Logf("  tools: %v  evidence: %d", out.ToolsUsed(), out.EvidenceCount())
	t.Logf("  affected_service: %q", out.AffectedService)
	t.Logf("  root_cause: %s", firstLine(out.RootCause))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
