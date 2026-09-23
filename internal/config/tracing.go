package config

import "fmt"

// Tracing configures the OTLP trace exporter.
//
// The endpoint is the switch: unset means tracing is a no-op, which is what
// lets every test and every `go run` work without a collector.
type Tracing struct {
	Endpoint string
	Service  string
}

// LoadTracing reads the trace configuration for the named service.
//
// The service name is an argument rather than an environment variable because
// it identifies the binary, and a binary that could be told it was a different
// one would produce traces nobody could read. OTEL_SERVICE_NAME still overrides
// it, since that is the variable every OTel tool expects to work.
func LoadTracing(service string) (Tracing, error) {
	var e env

	t := Tracing{
		Endpoint: e.optionalString("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		Service:  e.optionalString("OTEL_SERVICE_NAME", service),
	}
	if t.Endpoint != "" {
		if err := checkAbsoluteHTTP(t.Endpoint); err != nil {
			e.fail("OTEL_EXPORTER_OTLP_ENDPOINT %v", err)
		}
	}
	if t.Service == "" {
		e.fail("OTEL_SERVICE_NAME must not be empty")
	}

	if err := e.err(); err != nil {
		return Tracing{}, err
	}
	return t, nil
}

// String renders the configuration for startup logging.
func (t Tracing) String() string {
	if t.Endpoint == "" {
		return fmt.Sprintf("service=%s tracing=disabled", t.Service)
	}
	return fmt.Sprintf("service=%s endpoint=%s", t.Service, t.Endpoint)
}
