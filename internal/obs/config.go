package obs

// Config is what Setup needs. It is filled by config.LoadTracing rather than
// read here, so this package has no opinion about environment variables and
// internal/config keeps its rule of importing nothing from this project.
type Config struct {
	// Endpoint is the OTLP/HTTP collector URL. Empty disables tracing.
	Endpoint string

	// Service names this binary in a trace.
	Service string
}
