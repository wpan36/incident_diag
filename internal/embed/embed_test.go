package embed

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
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/log"
)

// clientFor points a client at a test server. MaxRetries is zero unless a test
// is about retrying, so that a test which is not about the schedule never waits
// for it.
func clientFor(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(config.Embedding{
		BaseURL:   srv.URL + "/v1",
		APIKey:    "test-key",
		Model:     "BAAI/bge-m3",
		BatchSize: 32,
		Timeout:   5 * time.Second,
	}, log.Discard())
}

// vector returns a distinguishable vector of the right length: every element is
// the same value, so a test can say which text a returned vector came from.
func vector(marker float32) []float32 {
	v := make([]float32, Dimensions)
	for i := range v {
		v[i] = marker
	}
	return v
}

func respond(w http.ResponseWriter, items ...map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
}

func TestTheRequestCarriesTheModelAndTheKey(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody embedRequest
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		respond(w, map[string]any{"index": 0, "embedding": vector(1)})
	})

	if _, err := c.Embed(context.Background(), []string{"one"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotPath != "/v1/embeddings" {
		t.Errorf("path = %q, want /v1/embeddings", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotBody.Model != "BAAI/bge-m3" || gotBody.EncodingFormat != "float" {
		t.Errorf("request = %+v", gotBody)
	}
}

// TestAResponseOutOfOrderIsRePaired is the check that earns its keep: pairing
// vector i with chunk j produces an index that is wrong in a way nothing
// downstream can detect.
func TestAResponseOutOfOrderIsRePaired(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(w,
			map[string]any{"index": 2, "embedding": vector(3)},
			map[string]any{"index": 0, "embedding": vector(1)},
			map[string]any{"index": 1, "embedding": vector(2)},
		)
	})

	got, err := c.Embed(context.Background(), []string{"one", "two", "three"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	for i, want := range []float32{1, 2, 3} {
		if got[i][0] != want {
			t.Errorf("vector %d starts with %v, want %v: the response was not re-ordered by index", i, got[i][0], want)
		}
	}
}

func TestAShortResponseIsAnError(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		respond(w, map[string]any{"index": 0, "embedding": vector(1)})
	})
	c.cfg.MaxRetries = 2

	_, err := c.Embed(context.Background(), []string{"one", "two"})
	if !errors.Is(err, ErrBadResponse) {
		t.Fatalf("err = %v, want ErrBadResponse", err)
	}
	// A provider that is not speaking the protocol will not start doing so on
	// the second attempt.
	if calls.Load() != 1 {
		t.Errorf("the request was attempted %d times, want 1", calls.Load())
	}
}

func TestADuplicateIndexIsAnError(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		respond(w,
			map[string]any{"index": 0, "embedding": vector(1)},
			map[string]any{"index": 0, "embedding": vector(2)},
		)
	})
	if _, err := c.Embed(context.Background(), []string{"one", "two"}); !errors.Is(err, ErrBadResponse) {
		t.Fatalf("err = %v, want ErrBadResponse", err)
	}
}

// TestAWrongLengthVectorIsAPermanentFailure covers the hazard ADR 0006 names
// and that nothing else in this system detects: EMBEDDING_MODEL changed without
// a reindex.
func TestAWrongLengthVectorIsAPermanentFailure(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		respond(w, map[string]any{"index": 0, "embedding": make([]float32, 768)})
	})
	c.cfg.MaxRetries = 2

	_, err := c.Embed(context.Background(), []string{"one"})
	var dimErr *DimensionError
	if !errors.As(err, &dimErr) {
		t.Fatalf("err = %v, want a DimensionError", err)
	}
	if dimErr.Got != 768 || dimErr.Want != Dimensions {
		t.Errorf("err = %v, want it to name 768 and %d", dimErr, Dimensions)
	}
	if calls.Load() != 1 {
		t.Errorf("the request was attempted %d times, want 1", calls.Load())
	}
}

func TestA400IsNotRetried(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":"input is too long"}`, http.StatusBadRequest)
	})
	c.cfg.MaxRetries = 3

	_, err := c.Embed(context.Background(), []string{"one"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("err = %v, want an APIError carrying 400", err)
	}
	if !strings.Contains(err.Error(), "input is too long") {
		t.Errorf("err = %v, want the provider's message preserved for failure_reason", err)
	}
	if calls.Load() != 1 {
		t.Errorf("the request was attempted %d times, want 1: an input too long is too long every time", calls.Load())
	}
}

// TestAnInputTooLongIsNotRetriedWhateverTheStatus: SiliconFlow says 400, a
// self-hosted TEI or vLLM behind the same OpenAI-compatible URL says 413 or
// 422, and the reason to give up is the same in all three.
func TestAnInputTooLongIsNotRetriedWhateverTheStatus(t *testing.T) {
	for _, status := range []int{http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				http.Error(w, "input is too long", status)
			})
			c.cfg.MaxRetries = 3

			_, err := c.Embed(context.Background(), []string{"one"})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != status {
				t.Fatalf("err = %v, want an APIError carrying %d", err, status)
			}
			if calls.Load() != 1 {
				t.Errorf("the request was attempted %d times, want 1", calls.Load())
			}
		})
	}
}

// TestAMissingKeyIsRetried states the deliberate choice: 401 and 403 look
// permanent and are treated as transient, so that a backlog heals itself once
// an operator fixes the key rather than having to be re-uploaded by hand.
func TestAMissingKeyIsRetried(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		respond(w, map[string]any{"index": 0, "embedding": vector(1)})
	})
	c.cfg.MaxRetries = 1

	if _, err := c.Embed(context.Background(), []string{"one"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("the request was attempted %d times, want 2", calls.Load())
	}
}

func TestRetriesAreBounded(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, "upstream is unwell", http.StatusServiceUnavailable)
	})
	c.cfg.MaxRetries = 2

	if _, err := c.Embed(context.Background(), []string{"one"}); err == nil {
		t.Fatal("Embed succeeded against a server that never does")
	}
	if want := int32(3); calls.Load() != want {
		t.Errorf("the request was attempted %d times, want %d (one attempt plus two retries)", calls.Load(), want)
	}
}

func TestTextsAreSentInBatchesAndComeBackInOrder(t *testing.T) {
	var batches atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		batches.Add(1)
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		items := make([]map[string]any, 0, len(req.Input))
		for i, text := range req.Input {
			// The marker identifies the text, so the assembled result can be
			// checked across batch boundaries.
			var marker float32
			_, _ = fmt.Sscanf(text, "text-%f", &marker)
			items = append(items, map[string]any{"index": i, "embedding": vector(marker)})
		}
		respond(w, items...)
	})
	c.cfg.BatchSize = 2

	texts := []string{"text-1", "text-2", "text-3", "text-4", "text-5"}
	got, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != len(texts) {
		t.Fatalf("got %d vectors for %d texts", len(got), len(texts))
	}
	if batches.Load() != 3 {
		t.Errorf("sent %d requests, want 3 for 5 texts at a batch size of 2", batches.Load())
	}
	for i := range texts {
		if want := float32(i + 1); got[i][0] != want {
			t.Errorf("vector %d starts with %v, want %v", i, got[i][0], want)
		}
	}
}

func TestRetryAfterIsHonouredAndCapped(t *testing.T) {
	cases := map[string]time.Duration{
		"":        0,
		"2":       2 * time.Second,
		"3600":    maxRetryDelay,
		"-1":      0,
		"Mon, 01": 0,
	}
	for header, want := range cases {
		h := http.Header{}
		if header != "" {
			h.Set("Retry-After", header)
		}
		if got := retryAfter(h); got != want {
			t.Errorf("retryAfter(%q) = %s, want %s", header, got, want)
		}
	}
}

func TestTheFakeIsDeterministicAndUnitLength(t *testing.T) {
	var f Fake
	got, err := f.Embed(context.Background(), []string{"alpha", "beta", "alpha"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got[0]) != Dimensions {
		t.Fatalf("vector length = %d, want %d", len(got[0]), Dimensions)
	}
	for i := range got[0] {
		if got[0][i] != got[2][i] {
			t.Fatal("the same text must produce the same vector")
		}
	}
	if got[0][0] == got[1][0] {
		t.Error("different texts should produce different vectors")
	}

	var norm float64
	for _, x := range got[0] {
		norm += float64(x) * float64(x)
	}
	if norm < 0.99 || norm > 1.01 {
		t.Errorf("squared norm = %v, want 1: a cosine search over fake vectors should behave like one over real ones", norm)
	}
}
