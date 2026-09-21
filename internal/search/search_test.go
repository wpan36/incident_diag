package search

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/log"
)

// clientFor points a client at a fake Elasticsearch.
//
// The tests that need a real cluster are in integration_test.go. What is worth
// faking here is the shape of a reply the real cluster almost never produces:
// a bulk request that answers 200 with its failures inside the body, and a
// delete-by-query that answers 200 having deleted nothing. Both are the cases
// where a status line alone would be believed.
func clientFor(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Without this header the v8 client refuses to talk to the server at
		// all, on the grounds that it might not be Elasticsearch.
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		h(w, r)
	}))
	t.Cleanup(srv.Close)

	c, err := New(config.Search{URL: srv.URL, IndexAlias: "chunks"}, log.Discard())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func testChunk(index int) Chunk {
	return Chunk{
		DocumentID: "01JBQ8M0YB4C3D2E1F0G9H8J7K",
		ChunkID:    ChunkID("01JBQ8M0YB4C3D2E1F0G9H8J7K", index),
		ChunkIndex: index,
		Content:    fmt.Sprintf("chunk %d", index),
		Embedding:  embed.FakeVector(fmt.Sprintf("chunk %d", index), embed.Dimensions),
	}
}

// bulkReply renders a bulk response in which the first item failed.
func bulkReply(status int, errType, reason string) string {
	return fmt.Sprintf(`{"errors":true,"items":[
	  {"index":{"status":%d,"error":{"type":%q,"reason":%q}}},
	  {"index":{"status":201}}
	]}`, status, errType, reason)
}

func TestChunkIDIsDeterministic(t *testing.T) {
	if got, want := ChunkID("doc", 7), "doc-7"; got != want {
		t.Errorf("ChunkID = %q, want %q", got, want)
	}
}

// TestTheMappingIsBuiltFromTheEmbeddingDimensions is what keeps the index and
// the client's own dimension check from drifting apart.
func TestTheMappingIsBuiltFromTheEmbeddingDimensions(t *testing.T) {
	c := clientFor(t, func(http.ResponseWriter, *http.Request) {})
	mapping := c.mapping()

	var decoded struct {
		Aliases  map[string]any `json:"aliases"`
		Mappings struct {
			Properties struct {
				Embedding struct {
					Type       string `json:"type"`
					Dims       int    `json:"dims"`
					Index      bool   `json:"index"`
					Similarity string `json:"similarity"`
				} `json:"embedding"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal([]byte(mapping), &decoded); err != nil {
		t.Fatalf("the mapping is not valid JSON: %v\n%s", err, mapping)
	}

	vec := decoded.Mappings.Properties.Embedding
	if vec.Dims != embed.Dimensions {
		t.Errorf("dims = %d, want embed.Dimensions (%d)", vec.Dims, embed.Dimensions)
	}
	// A dense_vector that is merely stored cannot be searched, and the failure
	// would only surface when retrieval is built.
	if !vec.Index || vec.Similarity != "cosine" {
		t.Errorf("embedding mapping = %+v, want an indexed cosine vector", vec)
	}
	if _, ok := decoded.Aliases["chunks"]; !ok {
		t.Errorf("the mapping does not point the alias at the new index: %s", mapping)
	}
}

// TestABulkItemRejectedForBackpressureIsRetried is the case the client's own
// RetryOnStatus cannot see: a bulk request answers 200 and puts the 429 in the
// body. Without this retry a full write queue would cost the document one of
// its INGEST_MAX_ATTEMPTS.
func TestABulkItemRejectedForBackpressureIsRetried(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			fmt.Fprint(w, bulkReply(http.StatusTooManyRequests,
				"es_rejected_execution_exception", "write queue is full"))
			return
		}
		fmt.Fprint(w, `{"errors":false,"items":[{"index":{"status":201}},{"index":{"status":201}}]}`)
	})

	if err := c.IndexChunks(context.Background(), []Chunk{testChunk(0), testChunk(1)}); err != nil {
		t.Fatalf("IndexChunks: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("the batch was sent %d times, want 2", calls.Load())
	}
}

// TestABulkMappingConflictIsNotRetried: the other half of the same decision.
// A mapping conflict will be a mapping conflict every time, and retrying it
// only delays the failure the document was always going to get.
func TestABulkMappingConflictIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, bulkReply(http.StatusBadRequest,
			"document_parsing_exception", "failed to parse field [embedding]"))
	})

	err := c.IndexChunks(context.Background(), []Chunk{testChunk(0), testChunk(1)})
	var itemErr *BulkItemError
	if !errors.As(err, &itemErr) {
		t.Fatalf("err = %v, want a BulkItemError", err)
	}
	if itemErr.Status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", itemErr.Status)
	}
	// Items come back in request order, so the error can say which chunk it
	// was — which is the difference between a reason a human can act on and
	// one they cannot.
	if itemErr.ChunkID != ChunkID("01JBQ8M0YB4C3D2E1F0G9H8J7K", 0) {
		t.Errorf("ChunkID = %q, want the first chunk of the batch", itemErr.ChunkID)
	}
	if calls.Load() != 1 {
		t.Errorf("the batch was sent %d times, want 1", calls.Load())
	}
}

func TestABulkRetryIsBounded(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, bulkReply(http.StatusTooManyRequests,
			"es_rejected_execution_exception", "write queue is full"))
	})

	if err := c.IndexChunks(context.Background(), []Chunk{testChunk(0), testChunk(1)}); err == nil {
		t.Fatal("IndexChunks succeeded against a cluster that never accepts anything")
	}
	if got := calls.Load(); got != bulkMaxAttempts {
		t.Errorf("the batch was sent %d times, want %d", got, bulkMaxAttempts)
	}
}

// TestADeleteThatLeftChunksBehindIsAnError: delete-by-query reports a failed
// shard in the body and answers 200 anyway, and conflicts=proceed skips a
// document that changed rather than aborting. Both leave chunks behind, and
// both would otherwise be read as success — by the delete that runs before
// every index so re-processing converges, and by the one that runs after a
// failed index so no document is left indexed at sixty percent.
func TestADeleteThatLeftChunksBehindIsAnError(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"a failed shard": {
			body: `{"deleted":0,"version_conflicts":0,"failures":[
			  {"cause":{"type":"shard_not_available_exception","reason":"node left"}}]}`,
			want: "shard_not_available_exception",
		},
		"a version conflict": {
			body: `{"deleted":3,"version_conflicts":2,"failures":[]}`,
			want: "2 chunks changed",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			client := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprint(w, c.body)
			})
			err := client.DeleteByDocument(context.Background(), "01JBQ8M0YB4C3D2E1F0G9H8J7K")
			if err == nil {
				t.Fatal("DeleteByDocument reported success although chunks were left behind")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

func TestACleanDeleteSucceeds(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"deleted":3,"version_conflicts":0,"failures":[]}`)
	})
	if err := c.DeleteByDocument(context.Background(), "01JBQ8M0YB4C3D2E1F0G9H8J7K"); err != nil {
		t.Errorf("DeleteByDocument: %v", err)
	}
}
