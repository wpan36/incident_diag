package config

import "fmt"

// Metrics is the workers' own listener.
//
// It exists because cmd/ingestion-worker and cmd/agent-worker have no HTTP
// server otherwise, and a process Prometheus cannot scrape is a process nobody
// can ask how it is doing. cmd/api and cmd/ops-mcp need none of this: they
// serve /metrics on the listener they already have.
type Metrics struct {
	Addr string
}

// LoadMetrics reads the listener address, defaulting per binary.
//
// The default is an argument rather than a constant because the two workers
// run side by side on a developer's machine and would otherwise fight over one
// port. METRICS_ADDR overrides it, which is what a second instance of the same
// worker needs.
func LoadMetrics(defaultAddr string) (Metrics, error) {
	var e env

	m := Metrics{Addr: e.optionalString("METRICS_ADDR", defaultAddr)}
	if m.Addr == "" {
		e.fail("METRICS_ADDR must not be empty")
	}

	if err := e.err(); err != nil {
		return Metrics{}, err
	}
	return m, nil
}

// String renders the configuration for startup logging.
func (m Metrics) String() string { return fmt.Sprintf("addr=%s", m.Addr) }
