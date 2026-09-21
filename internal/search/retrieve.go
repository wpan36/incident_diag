package search

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wpan36/incident_diag/internal/embed"
)

// Retrieval limits.
const (
	// DefaultK is how many chunks a search returns when the caller does not
	// say.
	DefaultK = 10

	// MaxK bounds it. The agent's context budget is the real constraint —
	// fifty chunks of four hundred tokens is twenty thousand tokens, already
	// more than a single step should ever be handed.
	MaxK = 50

	// minCandidates and maxCandidates bound the kNN graph traversal.
	//
	// num_candidates is how many neighbours each shard considers before the
	// top k are chosen: too low silently costs recall, while above the corpus
	// size it costs nothing at this scale. Being generous is the setting that
	// cannot quietly go wrong.
	minCandidates = 50
	maxCandidates = 1000
)

// Query is everything about a search except the vector.
//
// The filters are empty strings rather than pointers because "any" and "unset"
// are the same thing here: there is no query that means "chunks whose service
// is null", and inventing one would be a filter nothing asks for.
type Query struct {
	Service      string
	DocumentType string
	K            int
}

// Result is one retrieved chunk and how well it matched.
type Result struct {
	Chunk

	// Score is Elasticsearch's, which for a cosine kNN query is (1 + cosine)/2
	// and so lies in [0, 1]. It is carried for display and for debugging a
	// disappointing result; nothing in this project thresholds on it, because a
	// cutoff that is right for one query is wrong for the next.
	Score float64
}

// Search returns the chunks nearest to vector, most similar first.
//
// It takes a vector rather than text on purpose. This package depends on
// internal/embed for one constant and nothing else; holding an Embedder would
// put the retrieval path's timeout and retry policy here, while the failure it
// is most likely to hit — the hosted embedding provider being slow or rate
// limiting — belongs to a different dependency. Callers compose the two, which
// is what lets them tell an embedding failure from a search failure.
func (c *Client) Search(ctx context.Context, vector []float32, q Query) ([]Result, error) {
	// Checked here rather than left to Elasticsearch, whose error for a
	// dimension mismatch names neither the expected length nor the received
	// one.
	if len(vector) != embed.Dimensions {
		return nil, fmt.Errorf("search: query vector has %d dimensions, want %d",
			len(vector), embed.Dimensions)
	}

	body, err := searchBody(vector, q, clampK(q.K))
	if err != nil {
		return nil, err
	}

	res, err := c.es.Search(
		c.es.Search.WithContext(ctx),
		c.es.Search.WithIndex(c.alias),
		c.es.Search.WithBody(strings.NewReader(body)),
	)
	if err != nil {
		return nil, fmt.Errorf("search: query: %w", err)
	}
	defer res.Body.Close()

	if res.IsError() {
		return nil, responseError(res, "query")
	}

	var decoded searchResponse
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("search: decode the query response: %w", err)
	}

	// A shard that failed means the result is missing chunks the query would
	// have matched, and Elasticsearch reports that in the body of a 200. An
	// incomplete retrieval answered as a complete one is the kind of failure
	// that shows up much later as a diagnosis that missed the obvious runbook.
	if decoded.Shards.Failed > 0 {
		return nil, fmt.Errorf("search: query: %d of %d shards failed",
			decoded.Shards.Failed, decoded.Shards.Total)
	}

	out := make([]Result, 0, len(decoded.Hits.Hits))
	for _, h := range decoded.Hits.Hits {
		chunk := h.Source
		// The vector is not in _source for this query, but clearing it is
		// stated rather than assumed: a thousand floats per hit would dominate
		// every JSON response and every debugging print.
		chunk.Embedding = nil
		out = append(out, Result{Chunk: chunk, Score: h.Score})
	}
	return out, nil
}

// clampK resolves the caller's K.
//
// The HTTP endpoint already rejects a K outside the range rather than clamping
// it, for the reason parsePageParams gives: a silently reduced result looks
// complete. This is for the in-process callers that come later — the agent asks
// for chunks directly, and a bug in its budget arithmetic should cost a smaller
// result rather than a request Elasticsearch refuses.
func clampK(k int) int {
	switch {
	case k <= 0:
		return DefaultK
	case k > MaxK:
		return MaxK
	}
	return k
}

// searchBody builds the kNN request.
//
// The filters go inside the kNN clause rather than beside it, so they narrow
// the vector search itself. A post-filter would ask for the k nearest chunks
// overall and then discard the ones from other services, which returns fewer
// than k results — or none — exactly when the filter is doing something.
func searchBody(vector []float32, q Query, k int) (string, error) {
	candidates := 10 * k
	if candidates < minCandidates {
		candidates = minCandidates
	}
	if candidates > maxCandidates {
		candidates = maxCandidates
	}

	knn := map[string]any{
		"field":          "embedding",
		"query_vector":   vector,
		"k":              k,
		"num_candidates": candidates,
	}
	if filter := termFilters(q); len(filter) > 0 {
		knn["filter"] = filter
	}

	body := map[string]any{
		"knn":  knn,
		"size": k,
		// The vector is excluded rather than the other fields listed, so a
		// field added to the mapping is returned without anyone remembering to
		// add it here too.
		"_source": map[string]any{"excludes": []string{"embedding"}},
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("search: encode the query: %w", err)
	}
	return string(encoded), nil
}

// termFilters turns the query's filters into term clauses.
func termFilters(q Query) []map[string]any {
	var out []map[string]any
	if q.Service != "" {
		out = append(out, map[string]any{"term": map[string]any{"service": q.Service}})
	}
	if q.DocumentType != "" {
		out = append(out, map[string]any{"term": map[string]any{"document_type": q.DocumentType}})
	}
	return out
}

// searchResponse is the part of a search reply this package reads.
type searchResponse struct {
	Shards struct {
		Total  int `json:"total"`
		Failed int `json:"failed"`
	} `json:"_shards"`
	Hits struct {
		Hits []struct {
			Score  float64 `json:"_score"`
			Source Chunk   `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}
