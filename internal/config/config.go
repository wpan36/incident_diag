// Package config loads service configuration from the process environment.
//
// The environment is the only source. Docker Compose supplies .env through its
// env_file directive and a local shell sources it directly, so the Go code has
// exactly one code path and behaves identically in both.
//
// Load reports every problem it finds at once and refuses to return a partly
// valid Config: a service should fail at startup with a complete explanation
// rather than fail later with a confusing one.
package config

import (
	"fmt"
	"log/slog"
)

// Environment names. These are compared case-insensitively and stored lowercase.
const (
	EnvDevelopment = "development"
	EnvProduction  = "production"
)

// Config is the configuration shared by every binary in this project.
//
// It holds only what something actually reads today. Fields are added by the
// milestone that needs them — the database DSN when the store lands, the Kafka
// brokers when the workers land — rather than declared in advance.
type Config struct {
	// Env selects environment-dependent behaviour. It is not a security
	// boundary; it only decides things like whether to log at debug level by
	// default.
	Env string

	// LogLevel is the minimum level emitted by the logger.
	LogLevel slog.Level

	// HTTPAddr is the listen address for the HTTP server, in Go's host:port
	// form. A leading colon means all interfaces.
	HTTPAddr string
}

// IsDevelopment reports whether the service is running in the development
// environment.
func (c Config) IsDevelopment() bool { return c.Env == EnvDevelopment }

// Load reads the configuration from the environment.
//
// The returned error, if any, describes every problem found, so callers should
// print it and exit rather than trying to recover.
func Load() (Config, error) {
	var e env

	cfg := Config{
		Env:      e.oneOf("APP_ENV", EnvDevelopment, EnvDevelopment, EnvProduction),
		HTTPAddr: e.optionalString("HTTP_ADDR", ":8080"),
	}

	// The default level depends on the environment, so it is resolved after
	// Env is known.
	defaultLevel := slog.LevelInfo
	if cfg.Env == EnvDevelopment {
		defaultLevel = slog.LevelDebug
	}
	cfg.LogLevel = e.level("LOG_LEVEL", defaultLevel)

	if err := e.err(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// level returns the value of key parsed as a slog level, or def if unset.
func (e *env) level(key string, def slog.Level) slog.Level {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	var l slog.Level
	if err := l.UnmarshalText([]byte(v)); err != nil {
		e.fail("%s must be one of debug, info, warn, error, got %q", key, v)
		return def
	}
	return l
}

// String renders the configuration for startup logging. It exists so that
// adding a secret-bearing field later is a deliberate act: secrets must be
// omitted here, never formatted with %+v at a call site.
func (c Config) String() string {
	return fmt.Sprintf("env=%s log_level=%s http_addr=%s", c.Env, c.LogLevel, c.HTTPAddr)
}
