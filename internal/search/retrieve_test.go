package search

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/wpan36/incident_diag/internal/embed"
)

// decodeBody parses what searchBody produced, so the assertions are about the
// request Elasticsearch will receive rather than about a string.
func decodeBody(t *testing.T, vector []float32, q Query, k int) map[string]any {
	t.Helper()
	raw, err := searchBody(vector, q, k)
	if err != nil {
		t.Fatalf("searchBody: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("searchBody produced invalid JSON: %v\n%s", err, raw)
	}
	return out
}

func testVector() []float32 { return make([]float32, embed.Dimensions) }

func TestSearchBodyExcludesTheVectorFromTheSource(t *testing.T) {
	body := decodeBody(t, testVector(), Query{}, 10)

	source, ok := body["_source"].(map[string]any)
	if !ok {
		t.Fatalf("_source = %v", body["_source"])
	}
	excludes, ok := source["excludes"].([]any)
	if !ok || len(excludes) != 1 || excludes[0] != "embedding" {
		t.Errorf("_source.excludes = %v, want [embedding]", source["excludes"])
	}
	// Excluding rather than listing the fields wanted is deliberate: a field
	// added to the mapping is then returned without anyone remembering to add
	// it here as well.
	if _, listed := source["includes"]; listed {
		t.Error("_source lists includes; a new mapping field would be silently dropped")
	}
}

func TestSearchBodyOmitsAbsentFilters(t *testing.T) {
	body := decodeBody(t, testVector(), Query{}, 10)
	knn := body["knn"].(map[string]any)
	if _, ok := knn["filter"]; ok {
		t.Errorf("knn.filter is present with no filters set: %v", knn["filter"])
	}
}

func TestSearchBodyPutsFiltersInsideTheKnnClause(t *testing.T) {
	body := decodeBody(t, testVector(), Query{Service: "payment-service", DocumentType: "runbook"}, 10)

	knn, ok := body["knn"].(map[string]any)
	if !ok {
		t.Fatalf("knn = %v", body["knn"])
	}
	// Inside the kNN clause, not beside it. A post-filter would ask for the k
	// nearest chunks overall and then discard the ones from other services,
	// returning fewer than k — or none — exactly when the filter is doing
	// something.
	filters, ok := knn["filter"].([]any)
	if !ok || len(filters) != 2 {
		t.Fatalf("knn.filter = %v, want two term clauses", knn["filter"])
	}
	if _, beside := body["query"]; beside {
		t.Error("a top-level query is present; the filters must narrow the kNN search itself")
	}

	got := map[string]string{}
	for _, f := range filters {
		term := f.(map[string]any)["term"].(map[string]any)
		for field, value := range term {
			got[field] = value.(string)
		}
	}
	if got["service"] != "payment-service" || got["document_type"] != "runbook" {
		t.Errorf("filters = %v", got)
	}
}

func TestSearchBodyCandidateBounds(t *testing.T) {
	cases := []struct {
		k    int
		want float64
	}{
		{1, minCandidates}, // 10 would cost recall; the floor applies
		{5, minCandidates}, // 50 is exactly the floor
		{10, 100},          // ten times k
		{MaxK, 500},        // still under the ceiling
	}
	for _, c := range cases {
		body := decodeBody(t, testVector(), Query{}, c.k)
		knn := body["knn"].(map[string]any)
		if got := knn["num_candidates"]; got != c.want {
			t.Errorf("k=%d: num_candidates = %v, want %v", c.k, got, c.want)
		}
		if got := knn["k"]; got != float64(c.k) {
			t.Errorf("k=%d: knn.k = %v", c.k, got)
		}
		if got := body["size"]; got != float64(c.k) {
			t.Errorf("k=%d: size = %v, want it to match k", c.k, got)
		}
	}
}

func TestSearchRejectsAWrongLengthVector(t *testing.T) {
	c := &Client{alias: "chunks"}
	for _, n := range []int{0, embed.Dimensions - 1, embed.Dimensions + 1} {
		_, err := c.Search(context.Background(), make([]float32, n), Query{})
		if err == nil {
			t.Fatalf("a %d-dimension vector was accepted", n)
		}
		// Checked here rather than left to Elasticsearch, whose error for this
		// names neither the expected length nor the received one.
		if !strings.Contains(err.Error(), "dimensions") {
			t.Errorf("error = %v, want it to name the dimension mismatch", err)
		}
	}
}

func TestClampK(t *testing.T) {
	cases := map[int]int{
		0:        DefaultK, // unset
		-1:       DefaultK,
		1:        1,
		DefaultK: DefaultK,
		MaxK:     MaxK,
		MaxK + 1: MaxK,
		10_000:   MaxK,
	}
	for in, want := range cases {
		if got := clampK(in); got != want {
			t.Errorf("clampK(%d) = %d, want %d", in, got, want)
		}
	}
}

// searchReply renders a search response with the given number of failed shards
// and one hit per chunk, scored in descending order.
//
// The hits carry their embedding although the real cluster leaves it out of
// _source for this query. That is the point: Search clears the field rather
// than trusting that the request asked for it to be absent.
func searchReply(t *testing.T, failedShards int, chunks ...Chunk) string {
	t.Helper()

	hits := make([]string, 0, len(chunks))
	for i, chunk := range chunks {
		source, err := json.Marshal(chunk)
		if err != nil {
			t.Fatalf("encoding the fake hit: %v", err)
		}
		hits = append(hits, fmt.Sprintf(`{"_score":%v,"_source":%s}`, 0.9-0.1*float64(i), source))
	}
	return fmt.Sprintf(`{"_shards":{"total":2,"failed":%d},"hits":{"hits":[%s]}}`,
		failedShards, strings.Join(hits, ","))
}

// TestSearchFailsWhenAShardFailed is the reply no integration test can provoke
// and no status line reveals: Elasticsearch answers 200 and names the failed
// shard inside the body.
//
// An incomplete retrieval answered as a complete one is the failure that shows
// up much later as a diagnosis that missed the obvious runbook, so it has to be
// an error rather than a short result.
func TestSearchFailsWhenAShardFailed(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, searchReply(t, 1, testChunk(0)))
	})

	results, err := c.Search(context.Background(), testVector(), Query{})
	if err == nil {
		t.Fatalf("a search with a failed shard returned %d results and no error", len(results))
	}
	if !strings.Contains(err.Error(), "1 of 2 shards failed") {
		t.Errorf("error = %v, want it to name how many shards failed", err)
	}
}

func TestSearchDecodesHitsAndClearsTheEmbedding(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		// Every read goes through the alias, never a concrete index.
		if r.URL.Path != "/chunks/_search" {
			t.Errorf("searched %q, want the alias", r.URL.Path)
		}
		fmt.Fprint(w, searchReply(t, 0, testChunk(0), testChunk(1)))
	})

	results, err := c.Search(context.Background(), testVector(), Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].ChunkID != testChunk(0).ChunkID || results[0].Content != "chunk 0" {
		t.Errorf("first result = %+v", results[0])
	}
	if results[0].Score != 0.9 {
		t.Errorf("Score = %v, want the cluster's 0.9", results[0].Score)
	}
	for _, r := range results {
		if r.Embedding != nil {
			t.Errorf("result %s carries its embedding back to the caller", r.ChunkID)
		}
	}
}

func TestSearchReportsWhatTheClusterSaid(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"type":"search_phase_execution_exception","reason":"all shards failed"}}`)
	})

	_, err := c.Search(context.Background(), testVector(), Query{})
	if err == nil {
		t.Fatal("a 500 from the cluster was not an error")
	}
	// The status and the body both, because "search failed" on its own sends
	// the reader back to the cluster to find out what it said.
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "all shards failed") {
		t.Errorf("error = %v, want it to carry the status and the reason", err)
	}
}
