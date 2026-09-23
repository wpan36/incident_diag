// Package embed is an OpenAI-compatible embeddings client.
//
// It is deliberately not an abstraction over embedding providers. The value
// here is in one place that knows the wire format, the batching, the retry
// budget and the two checks that make a wrong answer impossible to ignore: that
// every vector comes back paired with the text it was computed from, and that
// it has the dimensionality the Elasticsearch mapping was built for.
//
// Where a failure is permanent and where it is transient is expressed as a
// retry decision rather than as a flag on the error. A request retried until
// the budget is spent and one refused immediately both end the same way — the
// document is marked FAILED with a reason, and the reconciler decides whether
// to bring it back.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/obs"
)

// tracer is resolved through the global provider on every span, so holding it
// here does not depend on obs.Setup having run first.
var tracer = obs.Tracer("embed")

// Dimensions is the vector length this system indexes, and the length
// BAAI/bge-m3 produces.
//
// It is a constant rather than a variable read from the environment. A wrong
// environment value would silently build a wrong Elasticsearch mapping, which
// is the exact failure the dimension check below exists to catch, and changing
// the embedding model already means reindexing every document (ADR 0006).
// internal/search builds its mapping from this constant, so the mapping and the
// check cannot drift apart.
const Dimensions = 1024

// Embedder is what the ingestion handler depends on.
//
// The interface exists because that handler's tests need a deterministic fake,
// not because a second implementation is expected.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

const (
	baseRetryDelay = 500 * time.Millisecond
	maxRetryDelay  = 10 * time.Second
)

// ErrBadResponse marks a response that does not match the request: the wrong
// number of embeddings, or an index outside the batch. It is never retried,
// because it means the provider is not speaking the protocol this client
// implements.
var ErrBadResponse = errors.New("embed: the provider's response does not match the request")

// APIError is a non-2xx response from the provider.
type APIError struct {
	StatusCode int
	Body       string

	// retryAfter is what the provider asked for on a 429. It is unexported
	// because it is the retry schedule's business and nothing else's.
	retryAfter time.Duration
}

func (e *APIError) Error() string {
	return fmt.Sprintf("embed: the embedding API returned %d: %s", e.StatusCode, e.Body)
}

// DimensionError means the provider returned vectors of a length the index
// cannot hold. It is the check that catches EMBEDDING_MODEL having been changed
// without a reindex, a hazard nothing else in this system detects.
type DimensionError struct {
	Got  int
	Want int
}

func (e *DimensionError) Error() string {
	return fmt.Sprintf("embed: the provider returned %d-dimensional vectors, but the index holds %d "+
		"(changing EMBEDDING_MODEL requires reindexing every document)", e.Got, e.Want)
}

// Client is an Embedder backed by an OpenAI-compatible /v1/embeddings endpoint.
type Client struct {
	cfg    config.Embedding
	http   *http.Client
	logger *slog.Logger
}

// New builds a client. The HTTP client has no timeout of its own: the deadline
// is applied per attempt through the context, so that a retry gets a fresh one.
func New(cfg config.Embedding, logger *slog.Logger) *Client {
	return &Client{cfg: cfg, http: &http.Client{Transport: obs.Transport(nil)}, logger: logger}
}

// Embed returns one vector per text, in the order the texts were given.
//
// Texts are sent in batches of EMBED_BATCH_SIZE, sequentially: the provider
// rate-limits, and the batches of one document have no deadline of their own
// beyond the handler's.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	ctx, span := tracer.Start(ctx, "embed.texts",
		trace.WithAttributes(attribute.Int("incident_diag.texts", len(texts))))
	defer span.End()

	started := time.Now()
	defer func() { obs.EmbeddingDuration.Observe(time.Since(started).Seconds()) }()

	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.cfg.BatchSize {
		end := min(start+c.cfg.BatchSize, len(texts))
		vectors, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("embedding texts %d-%d: %w", start, end-1, err)
		}
		out = append(out, vectors...)
	}
	return out, nil
}

// embedBatch sends one request, retrying transient failures until the budget is
// spent.
func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			if err := sleep(ctx, retryDelay(attempt, lastErr)); err != nil {
				return nil, errors.Join(lastErr, err)
			}
		}

		vectors, err := c.request(ctx, texts)
		switch {
		case err == nil:
			return vectors, nil
		case !retryable(err):
			return nil, err
		case ctx.Err() != nil:
			// A cancelled or expired context is not the provider's fault and
			// will not improve with another attempt.
			return nil, err
		}

		lastErr = err
		// Only when another attempt actually follows: a line saying "retrying"
		// as the budget runs out is a line that misreports what happened.
		if attempt < c.cfg.MaxRetries {
			c.logger.WarnContext(ctx, "embedding request failed, retrying",
				"attempt", attempt+1, "max_attempts", c.cfg.MaxRetries+1, "error", err)
		}
	}
	return nil, fmt.Errorf("after %d retries: %w", c.cfg.MaxRetries, lastErr)
}

type embedRequest struct {
	Model          string   `json:"model"`
	Input          []string `json:"input"`
	EncodingFormat string   `json:"encoding_format"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// request performs one attempt and pairs the response back up with its input.
func (c *Client) request(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.cfg.Model, Input: texts, EncodingFormat: "float"})
	if err != nil {
		return nil, fmt.Errorf("embed: encoding the request: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embed: building the request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embed: calling the embedding API: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode/100 != 2 {
		// Bounded, because a provider answering an error with a page of HTML
		// would otherwise put all of it in a failure_reason column.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &APIError{
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(detail)),
			retryAfter: retryAfter(resp.Header),
		}
	}

	var decoded embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("embed: decoding the response: %w", err)
	}
	return pair(decoded, len(texts))
}

// pair re-orders the response by each embedding's index field.
//
// The OpenAI-compatible schema carries that field precisely because order is
// not promised, and pairing vector i with chunk j produces an index that is
// wrong in a way nothing downstream can detect: every write succeeds, every
// search returns something, and only the recall number is quietly poor.
func pair(resp embedResponse, want int) ([][]float32, error) {
	if len(resp.Data) != want {
		return nil, fmt.Errorf("%w: %d embeddings for %d texts", ErrBadResponse, len(resp.Data), want)
	}

	out := make([][]float32, want)
	for _, item := range resp.Data {
		switch {
		case item.Index < 0 || item.Index >= want:
			return nil, fmt.Errorf("%w: index %d in a batch of %d", ErrBadResponse, item.Index, want)
		case out[item.Index] != nil:
			return nil, fmt.Errorf("%w: index %d returned twice", ErrBadResponse, item.Index)
		case len(item.Embedding) != Dimensions:
			return nil, &DimensionError{Got: len(item.Embedding), Want: Dimensions}
		}
		out[item.Index] = item.Embedding
	}
	return out, nil
}

// retryable decides whether another attempt could plausibly succeed.
//
// The permanent set is the document's own fault, whichever status the provider
// spells it with: an input too long for the model will be too long every time.
// SiliconFlow answers 400, and a self-hosted TEI or vLLM behind the same
// OpenAI-compatible URL answers 413 or 422 — the reason to give up is the same
// in all three cases, and retrying the others only spends the budget before
// failing with the message it would have failed with immediately.
//
// 401, 403 and 404 look permanent and are retried anyway: they are a
// misconfiguration of the deployment rather than a property of the document, so
// the useful behaviour is for the backlog to heal itself once an operator fixes
// the key. Treating them as permanent would burn all three attempts of every
// document uploaded during the outage.
func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
			return false
		}
		return true
	}
	var dimErr *DimensionError
	if errors.As(err, &dimErr) {
		return false
	}
	if errors.Is(err, ErrBadResponse) {
		return false
	}
	// What is left is a transport failure, a timeout, or a body that could not
	// be decoded. All three are worth one more attempt.
	return true
}

// retryDelay is exponential backoff, unless the provider said how long to wait.
// There is no jitter: one client with one in-flight request at a time is not a
// thundering herd, and a deterministic schedule is one a test can assert.
func retryDelay(attempt int, lastErr error) time.Duration {
	var apiErr *APIError
	if errors.As(lastErr, &apiErr) && apiErr.retryAfter > 0 {
		return apiErr.retryAfter
	}
	return min(baseRetryDelay<<(attempt-1), maxRetryDelay)
}

// retryAfter reads the Retry-After header a provider sends with 429. Only the
// delta-seconds form is honoured; the HTTP-date form is rare enough that
// falling back to the backoff schedule beats a date parser nothing exercises.
// It is capped, so a provider asking for an hour does not park the handler past
// its own deadline.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	seconds, err := strconv.Atoi(v)
	if err != nil || seconds < 0 {
		return 0
	}
	return min(time.Duration(seconds)*time.Second, maxRetryDelay)
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
