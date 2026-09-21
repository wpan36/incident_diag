package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/search"
)

// getSearch issues a search against a router with no embedder and no search
// client behind it.
//
// Every test here is rejected by validation before a handler touches either,
// and leaving both nil makes that assumption loud: a test that started reaching
// a dependency would panic rather than quietly need Elasticsearch running.
func getSearch(t *testing.T, query string) (*httptest.ResponseRecorder, errorBody) {
	t.Helper()
	h := testRouter(t)
	return do(t, h, httptest.NewRequest(http.MethodGet, "/api/search?"+query, nil))
}

func TestSearchRequiresAQuery(t *testing.T) {
	for _, q := range []string{"", "q=", "q=%20%20%20"} {
		rec, body := getSearch(t, q)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%q: status = %d, want 400", q, rec.Code)
		}
		// A query of three spaces is absent, not present-and-empty: embedding
		// whitespace would spend a request to retrieve nothing in particular.
		if body.Error.Fields["q"] != "required" {
			t.Errorf("%q: fields[q] = %q, want required", q, body.Error.Fields["q"])
		}
	}
}

func TestSearchRejectsAnOversizedQuery(t *testing.T) {
	rec, body := getSearch(t, "q="+strings.Repeat("a", maxQueryLen+1))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body.Error.Fields["q"] != "too_long" {
		t.Errorf("fields[q] = %q, want too_long", body.Error.Fields["q"])
	}
}

func TestSearchRejectsABadK(t *testing.T) {
	cases := map[string]string{
		"k=many":                   "invalid_type",
		"k=0":                      "invalid_value",
		"k=-1":                     "invalid_value",
		"k=" + itoa(search.MaxK+1): "invalid_value",
	}
	for query, code := range cases {
		rec, body := getSearch(t, "q=pool+exhausted&"+query)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", query, rec.Code)
		}
		if body.Error.Fields["k"] != code {
			t.Errorf("%s: fields[k] = %q, want %q", query, body.Error.Fields["k"], code)
		}
	}
}

func TestSearchReportsEveryBadParameterAtOnce(t *testing.T) {
	rec, body := getSearch(t, "service=Payment_Service&document_type=diary&k=0")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	want := map[string]string{
		"q":             "required",
		"service":       "invalid_format",
		"document_type": "invalid_value",
		"k":             "invalid_value",
	}
	if len(body.Error.Fields) != len(want) {
		t.Fatalf("fields = %v, want all four reported at once", body.Error.Fields)
	}
	for field, code := range want {
		if body.Error.Fields[field] != code {
			t.Errorf("fields[%s] = %q, want %q", field, body.Error.Fields[field], code)
		}
	}
	if body.Error.RequestID == "" {
		t.Error("the error body carries no request_id")
	}
}

// esReply is one hit, in the shape Elasticsearch answers a kNN query with.
const esReply = `{"_shards":{"total":1,"failed":0},"hits":{"hits":[
  {"_score":0.86,"_source":{
    "document_id":"01JBQ8M0YB4C3D2E1F0G9H8J7K",
    "chunk_id":"01JBQ8M0YB4C3D2E1F0G9H8J7K-2",
    "chunk_index":2,
    "service":"payment-service",
    "document_type":"runbook",
    "source":"payment-latency-runbook.md",
    "heading_path":"Payment latency runbook > Connection pool",
    "content":"pool_wait_seconds rises faster than the time spent using a connection.",
    "indexed_at":"2026-09-21T00:00:00Z"
  }}
]}}`

// esEmpty is a kNN query that matched nothing, which is a normal answer.
const esEmpty = `{"_shards":{"total":1,"failed":0},"hits":{"hits":[]}}`

// searchRouter builds a router with both of the search endpoint's dependencies
// behind it: a deterministic embedder, and a fake Elasticsearch answering with
// es.
//
// The other tests in this file leave both nil because validation rejects their
// requests first. These are the ones that get past it, and what they are for is
// the part the nil router cannot reach: that the two failures are told apart.
func searchRouter(t *testing.T, embedder embed.Embedder, es http.HandlerFunc) http.Handler {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Without this header the v8 client refuses to talk to the server at
		// all, on the grounds that it might not be Elasticsearch.
		w.Header().Set("X-Elastic-Product", "Elasticsearch")
		w.Header().Set("Content-Type", "application/json")
		es(w, r)
	}))
	t.Cleanup(srv.Close)

	client, err := search.New(config.Search{URL: srv.URL, IndexAlias: "chunks"}, log.Discard())
	if err != nil {
		t.Fatalf("opening the search client: %v", err)
	}
	return NewServer(Deps{Embedder: embedder, Search: client, Logger: log.Discard()}).Router()
}

func replyWith(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, body) }
}

func TestSearchRendersWhatWasRetrieved(t *testing.T) {
	h := searchRouter(t, &embed.Fake{}, replyWith(esReply))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=pool+exhausted&k=3", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, rec.Body.String())
	}
	// One key, not two. The endpoint reuses list[T] without claiming a
	// pagination a kNN result cannot support.
	if _, paginated := body["next_cursor"]; paginated {
		t.Errorf("the response carries next_cursor: %s", rec.Body.String())
	}

	var items []map[string]any
	if err := json.Unmarshal(body["items"], &items); err != nil {
		t.Fatalf("items is not an array: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1: %s", len(items), rec.Body.String())
	}
	// The score is what makes a result list judgeable; the embedding is a
	// thousand floats nobody reading this endpoint's output wants.
	if items[0]["score"] != 0.86 {
		t.Errorf("score = %v, want the cluster's 0.86", items[0]["score"])
	}
	if _, carried := items[0]["embedding"]; carried {
		t.Errorf("the response carries the embedding: %s", rec.Body.String())
	}
	for _, field := range []string{"chunk_id", "document_id", "source", "heading_path", "content"} {
		if items[0][field] == nil || items[0][field] == "" {
			t.Errorf("%s is missing; a retrieved chunk cannot be attributed without it", field)
		}
	}
}

// TestSearchTellsItsTwoDependenciesApart is the whole reason search.Search
// takes a vector rather than text.
//
// "The embedding provider is rate limiting" and "Elasticsearch is down" are
// different operational problems, and a single 503 that does not say which one
// wastes the first minute of every investigation into it.
func TestSearchTellsItsTwoDependenciesApart(t *testing.T) {
	cases := []struct {
		name     string
		embedder embed.Embedder
		es       http.HandlerFunc
		want     string
	}{
		{
			name:     "the embedder is down",
			embedder: &embed.Fake{Err: errors.New("429 Too Many Requests")},
			es: func(http.ResponseWriter, *http.Request) {
				t.Error("Elasticsearch was queried although embedding failed")
			},
			want: "the embedding service is unavailable",
		},
		{
			// A 500 rather than a 503, although 503 is the more obvious way to
			// say "down": the client retries 503 three times with backoff, and
			// this test would then spend five seconds measuring the retry
			// policy instead of the message. What the retries do belongs to
			// internal/search.
			name:     "Elasticsearch fails",
			embedder: &embed.Fake{},
			es: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, `{"error":{"reason":"no node available"}}`)
			},
			want: "search is unavailable",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := searchRouter(t, tc.embedder, tc.es)
			rec, body := do(t, h, httptest.NewRequest(http.MethodGet, "/api/search?q=pool", nil))

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
			}
			if body.Error.Code != "unavailable" {
				t.Errorf("code = %q, want unavailable", body.Error.Code)
			}
			if body.Error.Message != tc.want {
				t.Errorf("message = %q, want %q", body.Error.Message, tc.want)
			}
		})
	}
}

// TestSearchMeasuresTheQueryInCharacters is the same rule the rest of this
// package follows: a byte count would reject a 400-character Chinese question
// and tell it, in so many words, that it was longer than 1024 characters.
func TestSearchMeasuresTheQueryInCharacters(t *testing.T) {
	h := searchRouter(t, &embed.Fake{}, replyWith(esEmpty))

	atLimit := strings.Repeat("池", maxQueryLen)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/search?q="+url.QueryEscape(atLimit), nil))
	if rec.Code != http.StatusOK {
		t.Errorf("a %d-character query was rejected: %d %s",
			utf8.RuneCountInString(atLimit), rec.Code, rec.Body.String())
	}

	rec, body := do(t, h, httptest.NewRequest(http.MethodGet,
		"/api/search?q="+url.QueryEscape(atLimit+"池"), nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 one character over the limit", rec.Code)
	}
	if body.Error.Fields["q"] != "too_long" {
		t.Errorf("fields[q] = %q, want too_long", body.Error.Fields["q"])
	}
}
