package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/summary"
)

func knowledgeFor(hits ...search.Result) (*Knowledge, *fakeSearch) {
	s := &fakeSearch{hits: hits}
	return &Knowledge{Embedder: &embed.Fake{}, Search: s}, s
}

func retrieveWith(t *testing.T, k *Knowledge, args map[string]any) Observation {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encoding the arguments: %v", err)
	}
	return k.retrieve(context.Background(), raw)
}

// The bracketed value is the document_id, which is what finish cites.
func TestRetrieveRendersOneBlockPerHit(t *testing.T) {
	k, _ := knowledgeFor(
		hit("doc-1", "payment-runbook.md", "Symptoms > Rising latency", "the pool saturates", 0.8312),
		hit("doc-2", "payment-service.md", "", "payment-service charges cards", 0.51),
	)

	obs := retrieveWith(t, k, map[string]any{"query": "payment latency"})

	if obs.Error != "" {
		t.Fatalf("Error = %q", obs.Error)
	}
	want := "[doc-1] payment-runbook.md · Symptoms > Rising latency (0.83)\nthe pool saturates\n\n" +
		"[doc-2] payment-service.md (0.51)\npayment-service charges cards"
	if obs.Text != want {
		t.Errorf("Text =\n%s\n\nwant\n%s", obs.Text, want)
	}
	if len(obs.Hits) != 2 || obs.Query != "payment latency" {
		t.Errorf("the hits and the query must be kept for evidence: %+v", obs)
	}
}

// k defaults to 5, not search.DefaultK: ten chunks do not fit in an 8 KiB
// observation and five do. It is resolved here because search.clampK reads a
// zero as "unset" and answers with its own default.
func TestRetrieveDefaultsKToFive(t *testing.T) {
	k, s := knowledgeFor()

	retrieveWith(t, k, map[string]any{"query": "pool"})
	if s.last.K != defaultK {
		t.Errorf("K = %d, want %d", s.last.K, defaultK)
	}

	retrieveWith(t, k, map[string]any{"query": "pool", "k": -3})
	if s.last.K != defaultK {
		t.Errorf("a negative k gave K = %d, want %d", s.last.K, defaultK)
	}

	// Anything above search.MaxK is left to internal/search, which clamps it.
	retrieveWith(t, k, map[string]any{"query": "pool", "k": search.MaxK + 100})
	if s.last.K != search.MaxK+100 {
		t.Errorf("K = %d, want it passed through to internal/search", s.last.K)
	}
}

func TestRetrievePassesTheFilters(t *testing.T) {
	k, s := knowledgeFor()

	retrieveWith(t, k, map[string]any{
		"query": "pool", "service": "payment-service", "document_type": "runbook",
	})
	if s.last.Service != "payment-service" || s.last.DocumentType != "runbook" {
		t.Errorf("query = %+v", s.last)
	}
}

// Hits are dropped whole from the end: cutting mid-hit would hand the model
// half a chunk and a document id it cannot use.
func TestRetrieveDropsWholeHitsToFitTheCap(t *testing.T) {
	big := strings.Repeat("x", 3<<10)
	k, _ := knowledgeFor(
		hit("doc-1", "a.md", "", big, 0.9),
		hit("doc-2", "b.md", "", big, 0.8),
		hit("doc-3", "c.md", "", big, 0.7),
	)

	obs := retrieveWith(t, k, map[string]any{"query": "pool"})

	if len(obs.Text) > summary.LimitBytes {
		t.Errorf("the observation is %d bytes, over the %d-byte cap", len(obs.Text), summary.LimitBytes)
	}
	assertContains(t, obs.Text, "(2 of 3 hits shown; the rest did not fit)", "the observation")
	if strings.Contains(obs.Text, "[doc-3]") {
		t.Error("the dropped hit is still partly present")
	}
	// The hits themselves are all kept: what was dropped is the rendering, and
	// a citation of doc-3 would still be a citation of a document this step
	// returned.
	if len(obs.Hits) != 3 {
		t.Errorf("Hits = %d, want 3", len(obs.Hits))
	}
}

func TestRetrieveReportsNoMatches(t *testing.T) {
	k, _ := knowledgeFor()

	obs := retrieveWith(t, k, map[string]any{"query": "something nobody wrote about"})
	if obs.Error != "" || obs.Text != "No documents matched the query." {
		t.Errorf("observation = %+v", obs)
	}
}

// A failed retrieval is not a failed run: the failure text is the observation
// and the reason goes in the step's error.
func TestRetrieveRefusesABlankQuery(t *testing.T) {
	k, s := knowledgeFor()

	obs := retrieveWith(t, k, map[string]any{"query": "   "})
	if obs.Error == "" || obs.Text != obs.Error {
		t.Errorf("observation = %+v", obs)
	}
	if s.last.K != 0 {
		t.Error("a blank query reached the search client")
	}
}

func TestRetrieveReportsAnEmbeddingFailure(t *testing.T) {
	k, _ := knowledgeFor()
	k.Embedder = &embed.Fake{Err: errAlwaysFails}

	obs := k.retrieve(context.Background(), json.RawMessage(`{"query":"pool"}`))
	assertContains(t, obs.Error, "the search could not be run", "the observation")
	if obs.Query != "pool" {
		t.Errorf("Query = %q, want it kept for the citation fallback", obs.Query)
	}
}
