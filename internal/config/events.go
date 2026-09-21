package config

import (
	"fmt"
	"time"
)

// Events is the run event bus: one Redis stream per run.
//
// It is its own loader rather than part of LoadAgentWorker because two
// binaries need it — the agent worker writes these streams (M24) and the API
// reads them for SSE (M26) — and loading is split by concern, not by binary.
// It is not part of Load either, or cmd/migrate and cmd/ops-mcp would need a
// Redis to start.
type Events struct {
	// URL is the whole connection string, so the host, the port and any
	// password are one variable rather than three.
	URL string

	// PublishTimeout bounds one XADD. Without it an unresponsive Redis would
	// block a run inside the call whose whole point is that its failure does
	// not matter.
	PublishTimeout time.Duration

	// StreamMaxLen caps a run's stream. It is an approximate trim, and it is
	// far above what a run can produce: AGENT_MAX_STEPS is a single digit, so
	// the whole of a run's history is always present and a client can replay
	// it from the start.
	StreamMaxLen int

	// StreamTTL is how long a run's stream outlives its last event. It is
	// refreshed on every publish.
	StreamTTL time.Duration
}

// LoadEvents reads the event bus configuration from the environment,
// reporting every problem it finds at once.
func LoadEvents() (Events, error) {
	var e env

	c := Events{
		URL:            e.requiredString("REDIS_URL"),
		PublishTimeout: e.optionalDuration("REDIS_PUBLISH_TIMEOUT", 2*time.Second),
		StreamMaxLen:   e.optionalInt("EVENT_STREAM_MAXLEN", 1000),
		StreamTTL:      e.optionalDuration("EVENT_STREAM_TTL", 24*time.Hour),
	}

	if c.PublishTimeout <= 0 {
		e.fail("REDIS_PUBLISH_TIMEOUT must be greater than zero")
	}
	if c.StreamMaxLen < 1 {
		e.fail("EVENT_STREAM_MAXLEN must be at least 1, got %d", c.StreamMaxLen)
	}
	if c.StreamTTL <= 0 {
		e.fail("EVENT_STREAM_TTL must be greater than zero")
	}

	if err := e.err(); err != nil {
		return Events{}, err
	}
	return c, nil
}

// String renders the configuration for startup logging.
//
// The URL may carry a password, so it is not printed. Unlike a MySQL DSN it
// never reaches an error message either, because redis.ParseURL reports what
// is wrong with a connection string without quoting it.
func (c Events) String() string {
	return fmt.Sprintf("redis=<redacted> publish_timeout=%s stream_maxlen=%d stream_ttl=%s",
		c.PublishTimeout, c.StreamMaxLen, c.StreamTTL)
}
