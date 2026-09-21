//go:build integration

// Integration tests for the chunk index. They need Elasticsearch:
//
//	make up && make test-integration
//
// Every test works in an index alias of its own, so they cannot see each
// other's documents and a failing one leaves nothing behind for the next run.
package search

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/log"
)

func testClient(t *testing.T) *Client {
	t.Helper()

	url := os.Getenv("TEST_ELASTICSEARCH_URL")
	if url == "" {
		t.Skip("TEST_ELASTICSEARCH_URL is not set; skipping the integration tests")
	}

	alias := "test_" + strings.ToLower(id.New())
	c, err := New(config.Search{URL: url, IndexAlias: alias}, log.Discard())
	if err != nil {
		t.Fatalf("opening the client: %v", err)
	}
	// Named explicitly rather than with a wildcard: Elasticsearch refuses
	// wildcard deletes by default (action.destructive_requires_name), and a
	// cleanup that silently does nothing leaves a cluster full of test indices.
	t.Cleanup(func() {
		res, err := c.es.Indices.Delete([]string{alias, alias + "_v1", alias + "_v2"},
			c.es.Indices.Delete.WithIgnoreUnavailable(true))
		if err == nil {
			res.Body.Close()
		}
	})
	return c
}

func chunkFor(documentID string, index int, content string) Chunk {
	return Chunk{
		DocumentID:   documentID,
		ChunkID:      ChunkID(documentID, index),
		ChunkIndex:   index,
		DocumentType: "runbook",
		Source:       "runbook.md",
		HeadingPath:  "Payment Service > Latency",
		Content:      content,
		Embedding:    embed.FakeVector(content, embed.Dimensions),
		IndexedAt:    time.Now().UTC(),
	}
}

// count returns how many chunks of a document are searchable.
func count(t *testing.T, c *Client, documentID string) int {
	t.Helper()

	query := fmt.Sprintf(`{"query":{"term":{"document_id":%q}}}`, documentID)
	res, err := c.es.Count(
		c.es.Count.WithContext(context.Background()),
		c.es.Count.WithIndex(c.alias),
		c.es.Count.WithBody(strings.NewReader(query)),
	)
	if err != nil {
		t.Fatalf("counting: %v", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		t.Fatalf("counting: %s", res.String())
	}

	var decoded struct {
		Count int `json:"count"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		t.Fatalf("decoding the count: %v", err)
	}
	return decoded.Count
}

// TestEnsureIndexCreatesAndIsIdempotent is the property every worker startup
// depends on: calling it is never a mistake, however many workers do.
func TestEnsureIndexCreatesAndIsIdempotent(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := c.EnsureIndex(ctx); err != nil {
			t.Fatalf("EnsureIndex call %d: %v", i+1, err)
		}
	}

	target, err := c.resolveAlias(ctx)
	if err != nil {
		t.Fatalf("resolveAlias: %v", err)
	}
	if want := c.alias + "_v1"; target != want {
		t.Errorf("the alias points at %q, want %q", target, want)
	}
}

// TestEnsureIndexAcceptsAnAliasPointingAtV2 is what keeps the atomic reindex
// switch from bricking the next worker restart: any single index behind the
// alias is accepted, whatever it is called.
func TestEnsureIndexAcceptsAnAliasPointingAtV2(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	v2 := c.alias + "_v2"
	if err := c.createIndex(ctx, v2); err != nil {
		t.Fatalf("creating %s: %v", v2, err)
	}
	if err := c.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	target, err := c.resolveAlias(ctx)
	if err != nil {
		t.Fatalf("resolveAlias: %v", err)
	}
	if target != v2 {
		t.Errorf("the alias points at %q, want %q — v1 must not have been created alongside it", target, v2)
	}
}

// TestEnsureIndexRefusesAnAliasThatFansOut: writes going somewhere the operator
// did not intend, while startup looks healthy, is the case worth failing on.
func TestEnsureIndexRefusesAnAliasThatFansOut(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	for _, suffix := range []string{"_v1", "_v2"} {
		if err := c.createIndex(ctx, c.alias+suffix); err != nil {
			t.Fatalf("creating %s: %v", c.alias+suffix, err)
		}
	}

	err := c.EnsureIndex(ctx)
	if err == nil {
		t.Fatal("EnsureIndex accepted an alias resolving to two indices")
	}
	if !strings.Contains(err.Error(), "2 indices") {
		t.Errorf("err = %v, want it to say how many indices the alias resolves to", err)
	}
}

// TestEnsureIndexRefusesAConcreteIndexWithTheAliasName covers the other
// ambiguity: a plain index called "chunks" answers the alias lookup with itself,
// so writes would appear to work and the reindex switch would be impossible.
func TestEnsureIndexRefusesAConcreteIndexWithTheAliasName(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()

	res, err := c.es.Indices.Create(c.alias, c.es.Indices.Create.WithContext(ctx))
	if err != nil {
		t.Fatalf("creating a concrete index: %v", err)
	}
	res.Body.Close()

	err = c.EnsureIndex(ctx)
	if err == nil {
		t.Fatal("EnsureIndex accepted a concrete index wearing the alias name")
	}
	if !strings.Contains(err.Error(), "concrete index") {
		t.Errorf("err = %v, want it to name the problem", err)
	}
}

// TestIndexedChunksAreImmediatelySearchable is what refresh=wait_for buys. It
// is not a nicety: the cleanup path deletes by query, and delete-by-query only
// sees what is searchable.
func TestIndexedChunksAreImmediatelySearchable(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if err := c.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	documentID := id.New()
	chunks := []Chunk{
		chunkFor(documentID, 0, "Raise the connection pool size to 50."),
		chunkFor(documentID, 1, "Set the client timeout to 2 seconds."),
	}
	if err := c.IndexChunks(ctx, chunks); err != nil {
		t.Fatalf("IndexChunks: %v", err)
	}
	if got := count(t, c, documentID); got != 2 {
		t.Fatalf("%d chunks are searchable immediately after the bulk, want 2", got)
	}
}

// TestReIndexingTheSameDocumentDoesNotDuplicate is the property the
// deterministic chunk id exists for, and the one S2 relies on when it says
// re-processing converges rather than duplicating chunks.
func TestReIndexingTheSameDocumentDoesNotDuplicate(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if err := c.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	documentID := id.New()
	chunks := []Chunk{chunkFor(documentID, 0, "first"), chunkFor(documentID, 1, "second")}

	for i := 0; i < 2; i++ {
		if err := c.IndexChunks(ctx, chunks); err != nil {
			t.Fatalf("IndexChunks round %d: %v", i+1, err)
		}
	}
	if got := count(t, c, documentID); got != 2 {
		t.Errorf("%d chunks after indexing twice, want 2", got)
	}
}

func TestDeleteByDocumentRemovesOnlyThatDocument(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if err := c.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	mine, other := id.New(), id.New()
	if err := c.IndexChunks(ctx, []Chunk{
		chunkFor(mine, 0, "mine one"), chunkFor(mine, 1, "mine two"),
		chunkFor(other, 0, "someone else's"),
	}); err != nil {
		t.Fatalf("IndexChunks: %v", err)
	}

	if err := c.DeleteByDocument(ctx, mine); err != nil {
		t.Fatalf("DeleteByDocument: %v", err)
	}
	if got := count(t, c, mine); got != 0 {
		t.Errorf("%d chunks of the deleted document remain", got)
	}
	if got := count(t, c, other); got != 1 {
		t.Errorf("%d chunks of the other document remain, want 1", got)
	}
}

// TestDeleteByDocumentOnAnEmptyIndexSucceeds: the delete runs before every
// index, including the first one a document ever has.
func TestDeleteByDocumentOnAnEmptyIndexSucceeds(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if err := c.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	if err := c.DeleteByDocument(ctx, id.New()); err != nil {
		t.Errorf("DeleteByDocument on a document with no chunks: %v", err)
	}
}

// TestAWrongLengthVectorIsRejectedByTheMapping proves the mapping is really
// built from embed.Dimensions, and that a bulk item failing is reported rather
// than swallowed — a bulk request answers 200 with its failures inside the body.
func TestAWrongLengthVectorIsRejectedByTheMapping(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if err := c.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	documentID := id.New()
	chunk := chunkFor(documentID, 0, "wrong size")
	chunk.Embedding = make([]float32, 8)

	err := c.IndexChunks(ctx, []Chunk{chunk})
	if err == nil {
		t.Fatal("IndexChunks accepted an 8-dimensional vector into a 1024-dimensional field")
	}
	if got := count(t, c, documentID); got != 0 {
		t.Errorf("%d chunks were indexed anyway", got)
	}
}

// TestTheMappingIndexesTheVectorForKNN: a dense_vector that is merely stored
// cannot be searched, and the failure would only surface in M11.
func TestTheMappingIndexesTheVectorForKNN(t *testing.T) {
	c := testClient(t)
	ctx := context.Background()
	if err := c.EnsureIndex(ctx); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	documentID := id.New()
	target := "Raise the connection pool size to 50."
	if err := c.IndexChunks(ctx, []Chunk{
		chunkFor(documentID, 0, target),
		chunkFor(documentID, 1, "Something else entirely."),
	}); err != nil {
		t.Fatalf("IndexChunks: %v", err)
	}

	vector, err := json.Marshal(embed.FakeVector(target, embed.Dimensions))
	if err != nil {
		t.Fatalf("encoding the query vector: %v", err)
	}
	query := fmt.Sprintf(`{"knn":{"field":"embedding","query_vector":%s,"k":1,"num_candidates":10},"_source":["content"]}`, vector)

	res, err := c.es.Search(
		c.es.Search.WithContext(ctx),
		c.es.Search.WithIndex(c.alias),
		c.es.Search.WithBody(strings.NewReader(query)),
	)
	if err != nil {
		t.Fatalf("kNN search: %v", err)
	}
	defer res.Body.Close()
	if res.IsError() {
		t.Fatalf("kNN search: %s", res.String())
	}

	var decoded struct {
		Hits struct {
			Hits []struct {
				Source struct {
					Content string `json:"content"`
				} `json:"_source"`
			} `json:"hits"`
		} `json:"hits"`
	}
	if err := json.NewDecoder(res.Body).Decode(&decoded); err != nil {
		t.Fatalf("decoding the search response: %v", err)
	}
	if len(decoded.Hits.Hits) == 0 {
		t.Fatal("the kNN query returned nothing")
	}
	if got := decoded.Hits.Hits[0].Source.Content; got != target {
		t.Errorf("nearest neighbour is %q, want %q", got, target)
	}
}

// retrievableChunk is chunkFor with the fields the filters read, which the
// write tests do not set.
func retrievableChunk(documentID string, index int, service, documentType, content string) Chunk {
	c := chunkFor(documentID, index, content)
	c.Service = &service
	c.DocumentType = documentType
	c.Content = content
	c.Embedding = embed.FakeVector(content, embed.Dimensions)
	return c
}

// seedForSearch indexes three chunks that differ in both service and type, so
// every filter has something to exclude.
func seedForSearch(t *testing.T, c *Client) {
	t.Helper()
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}
	chunks := []Chunk{
		retrievableChunk(id.New(), 0, "payment-service", "runbook", "alpha"),
		retrievableChunk(id.New(), 0, "checkout-service", "runbook", "beta"),
		retrievableChunk(id.New(), 0, "payment-service", "postmortem", "gamma"),
	}
	if err := c.IndexChunks(context.Background(), chunks); err != nil {
		t.Fatalf("IndexChunks: %v", err)
	}
}

func TestSearchRanksTheNearestChunkFirst(t *testing.T) {
	c := testClient(t)
	seedForSearch(t, c)

	// FakeVector is deterministic, so querying with the vector of "alpha"
	// makes that chunk an exact match and the ranking predictable.
	results, err := c.Search(context.Background(), embed.FakeVector("alpha", embed.Dimensions), Query{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results, want all three", len(results))
	}
	if results[0].Content != "alpha" {
		t.Errorf("first result is %q, want alpha", results[0].Content)
	}
	if results[0].Score < results[len(results)-1].Score {
		t.Errorf("scores are ascending: %v then %v", results[0].Score, results[len(results)-1].Score)
	}
	// A thousand floats per hit would dominate every response and every
	// debugging print, so the vector never comes back.
	for _, r := range results {
		if r.Embedding != nil {
			t.Fatalf("result %s carries its embedding", r.ChunkID)
		}
	}
	// The fields a citation needs have to survive the round trip, or a
	// retrieved chunk cannot be attributed to a document.
	if results[0].ChunkID == "" || results[0].DocumentID == "" || results[0].Source == "" ||
		results[0].HeadingPath == "" || results[0].Service == nil {
		t.Errorf("a citation field is missing: %+v", results[0])
	}
}

func TestSearchFiltersNarrowTheResult(t *testing.T) {
	c := testClient(t)
	seedForSearch(t, c)
	ctx := context.Background()
	vector := embed.FakeVector("alpha", embed.Dimensions)

	cases := []struct {
		name  string
		query Query
		want  []string
	}{
		{"service", Query{Service: "payment-service"}, []string{"alpha", "gamma"}},
		{"document type", Query{DocumentType: "postmortem"}, []string{"gamma"}},
		{"both", Query{Service: "payment-service", DocumentType: "runbook"}, []string{"alpha"}},
		{"no match", Query{Service: "billing-service"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results, err := c.Search(ctx, vector, tc.query)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			got := make([]string, 0, len(results))
			for _, r := range results {
				got = append(got, r.Content)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			seen := map[string]bool{}
			for _, g := range got {
				seen[g] = true
			}
			for _, w := range tc.want {
				if !seen[w] {
					t.Errorf("got %v, want it to contain %q", got, w)
				}
			}
		})
	}
}

func TestSearchHonoursK(t *testing.T) {
	c := testClient(t)
	seedForSearch(t, c)

	results, err := c.Search(context.Background(),
		embed.FakeVector("alpha", embed.Dimensions), Query{K: 2})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
}

func TestSearchOnAnEmptyIndexReturnsNothing(t *testing.T) {
	c := testClient(t)
	if err := c.EnsureIndex(context.Background()); err != nil {
		t.Fatalf("EnsureIndex: %v", err)
	}

	// Nothing to match is a normal answer, not an error. The agent asks a
	// question before anything has been ingested at least once per deployment.
	results, err := c.Search(context.Background(),
		embed.FakeVector("alpha", embed.Dimensions), Query{})
	if err != nil {
		t.Fatalf("Search on an empty index: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results from an empty index", len(results))
	}
}
