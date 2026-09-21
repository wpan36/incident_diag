package config

import (
	"fmt"
	"time"
)

// LLM is the chat model the agent reasons with.
//
// It is a separate endpoint and a separate key from the embedding provider:
// ADR 0006 chose DeepSeek for chat, which has no embeddings endpoint at all.
// Nothing here names DeepSeek, which is the point — pointing these three
// variables at any other OpenAI-compatible provider is the whole of a
// provider switch.
type LLM struct {
	BaseURL string
	APIKey  string
	Model   string

	// Timeout bounds one attempt, not the sequence of retries. A run's total
	// LLM time is bounded by AGENT_MAX_STEPS and AGENT_MAX_RUN_DURATION
	// instead.
	Timeout time.Duration

	// MaxRetries is how many times a request may be retried after its first
	// attempt.
	MaxRetries int
}

// LoadLLM reads the chat model's configuration from the environment,
// reporting every problem it finds at once.
func LoadLLM() (LLM, error) {
	var e env

	c := LLM{
		BaseURL:    e.requiredString("LLM_BASE_URL"),
		APIKey:     e.requiredString("LLM_API_KEY"),
		Model:      e.requiredString("LLM_MODEL"),
		Timeout:    e.optionalDuration("LLM_TIMEOUT", 60*time.Second),
		MaxRetries: e.optionalInt("LLM_MAX_RETRIES", 2),
	}

	if c.Timeout <= 0 {
		e.fail("LLM_TIMEOUT must be greater than zero")
	}
	if c.MaxRetries < 0 {
		e.fail("LLM_MAX_RETRIES must not be negative, got %d", c.MaxRetries)
	}

	if err := e.err(); err != nil {
		return LLM{}, err
	}
	return c, nil
}

// String renders the configuration for startup logging with the key omitted.
func (c LLM) String() string {
	return fmt.Sprintf("base_url=%s api_key=<redacted> model=%s timeout=%s max_retries=%d",
		c.BaseURL, c.Model, c.Timeout, c.MaxRetries)
}
