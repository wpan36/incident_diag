package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
