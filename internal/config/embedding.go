package config

import (
	"fmt"
	"time"
)

// Embedding is the configuration for the embedding provider.
//
// It is a separate endpoint and a separate key from the chat LLM, because the
// provider that serves one need not serve the other: ADR 0006 records that the
// chat model is DeepSeek, which has no embeddings endpoint at all.
//
// The vector dimensionality is deliberately absent. It is a constant in
// internal/embed, from which internal/search builds its mapping — a wrong
// environment value would silently build a wrong mapping, which is the exact
// failure the dimension check exists to catch.
type Embedding struct {
	BaseURL string
	APIKey  string
	Model   string

	// BatchSize is how many texts go in one request.
	BatchSize int

	// Timeout bounds one request, not the whole batch sequence. A document of
	// 2000 chunks is 63 requests, and what bounds those together is
	// INGEST_DOCUMENT_TIMEOUT.
	Timeout time.Duration

	// MaxRetries is how many times a request may be retried after its first
	// attempt. Transient failures are retried inside the handler and become a
	// permanent failure once this budget is spent.
	MaxRetries int
}

// LoadEmbedding reads the embedding configuration from the environment,
// reporting every problem it finds at once.
func LoadEmbedding() (Embedding, error) {
	var e env

	c := Embedding{
		BaseURL:    e.requiredString("EMBEDDING_BASE_URL"),
		APIKey:     e.requiredString("EMBEDDING_API_KEY"),
		Model:      e.requiredString("EMBEDDING_MODEL"),
		BatchSize:  e.optionalInt("EMBED_BATCH_SIZE", 32),
		Timeout:    e.optionalDuration("EMBED_TIMEOUT", 30*time.Second),
		MaxRetries: e.optionalInt("EMBED_MAX_RETRIES", 3),
	}

	if c.BatchSize < 1 {
		e.fail("EMBED_BATCH_SIZE must be at least 1, got %d", c.BatchSize)
	}
	if c.Timeout <= 0 {
		e.fail("EMBED_TIMEOUT must be greater than zero")
	}
	if c.MaxRetries < 0 {
		e.fail("EMBED_MAX_RETRIES must not be negative, got %d", c.MaxRetries)
	}

	if err := e.err(); err != nil {
		return Embedding{}, err
	}
	return c, nil
}

// String renders the configuration for startup logging with the key omitted.
func (c Embedding) String() string {
	return fmt.Sprintf("base_url=%s api_key=<redacted> model=%s batch_size=%d timeout=%s max_retries=%d",
		c.BaseURL, c.Model, c.BatchSize, c.Timeout, c.MaxRetries)
}
