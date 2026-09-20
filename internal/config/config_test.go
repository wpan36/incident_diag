package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// isolate clears the variables Load reads, so a test is not affected by
// whatever the developer happens to have exported in their shell. Setting a
// variable to the empty string is equivalent to unsetting it here, because
// lookup treats blank as absent.
func isolate(t *testing.T) {
	t.Helper()
	for _, k := range []string{"APP_ENV", "HTTP_ADDR", "LOG_LEVEL"} {
		t.Setenv(k, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	isolate(t)
	// No variables set at all: every default applies and nothing fails.
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with an empty environment: %v", err)
	}
	if cfg.Env != EnvDevelopment {
		t.Errorf("Env = %q, want %q", cfg.Env, EnvDevelopment)
	}
	if !cfg.IsDevelopment() {
		t.Error("IsDevelopment() = false, want true")
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, ":8080")
	}
	// Development defaults to debug so that local runs are verbose without
	// anyone having to remember a flag.
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelDebug)
	}
}

func TestLoadProductionDefaultsToInfo(t *testing.T) {
	isolate(t)
	t.Setenv("APP_ENV", "production")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelInfo)
	}
	if cfg.IsDevelopment() {
		t.Error("IsDevelopment() = true in production")
	}
}

func TestLoadExplicitValues(t *testing.T) {
	isolate(t)
	t.Setenv("APP_ENV", "PRODUCTION") // case-insensitive, stored lowercase
	t.Setenv("HTTP_ADDR", "127.0.0.1:9000")
	t.Setenv("LOG_LEVEL", "warn")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Env != EnvProduction {
		t.Errorf("Env = %q, want %q", cfg.Env, EnvProduction)
	}
	if cfg.HTTPAddr != "127.0.0.1:9000" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.LogLevel != slog.LevelWarn {
		t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, slog.LevelWarn)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	isolate(t)
	t.Setenv("APP_ENV", "staging")
	t.Setenv("LOG_LEVEL", "chatty")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with two invalid values")
	}
	// The whole point of the collecting loader: one restart, all the problems.
	for _, want := range []string{"APP_ENV", "LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadReturnsZeroConfigOnError(t *testing.T) {
	isolate(t)
	t.Setenv("APP_ENV", "staging")

	cfg, err := Load()
	if err == nil {
		t.Fatal("Load() succeeded with an invalid APP_ENV")
	}
	// A partly valid Config is more dangerous than none: a caller that ignores
	// the error should not get something that looks usable.
	if cfg != (Config{}) {
		t.Errorf("Load() returned %+v alongside an error, want the zero value", cfg)
	}
}

func TestStringOmitsNothingKnownButIsExplicit(t *testing.T) {
	cfg := Config{Env: EnvProduction, LogLevel: slog.LevelInfo, HTTPAddr: ":8080"}
	got := cfg.String()
	for _, want := range []string{"env=production", "log_level=INFO", "http_addr=:8080"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, missing %q", got, want)
		}
	}
}

func TestBlankValueIsTreatedAsUnset(t *testing.T) {
	isolate(t)
	// A key left empty in a .env file is a common mistake. Falling back to the
	// default beats failing with "HTTP_ADDR must be host:port, got \"\"".
	t.Setenv("HTTP_ADDR", "   ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want the default", cfg.HTTPAddr)
	}
}

// The remaining helpers have no caller in Config yet; they are exercised
// directly so that the milestone adding their first field does not also have to
// debug them.

func TestRequiredString(t *testing.T) {
	t.Setenv("PRESENT", "value")

	var e env
	if got := e.requiredString("PRESENT"); got != "value" {
		t.Errorf("requiredString(PRESENT) = %q", got)
	}
	if got := e.requiredString("ABSENT"); got != "" {
		t.Errorf("requiredString(ABSENT) = %q, want empty", got)
	}
	err := e.err()
	if err == nil || !strings.Contains(err.Error(), "ABSENT is required") {
		t.Errorf("err() = %v, want a complaint about ABSENT", err)
	}
}

func TestOptionalScalars(t *testing.T) {
	t.Setenv("N", "42")
	t.Setenv("B", "true")
	t.Setenv("D", "1m30s")

	var e env
	if got := e.optionalInt("N", 7); got != 42 {
		t.Errorf("optionalInt = %d, want 42", got)
	}
	if got := e.optionalInt("MISSING", 7); got != 7 {
		t.Errorf("optionalInt default = %d, want 7", got)
	}
	if got := e.optionalBool("B", false); !got {
		t.Error("optionalBool = false, want true")
	}
	if got := e.optionalDuration("D", time.Second); got != 90*time.Second {
		t.Errorf("optionalDuration = %v, want 1m30s", got)
	}
	if err := e.err(); err != nil {
		t.Fatalf("err() = %v, want nil", err)
	}
}

func TestOptionalScalarsRejectMalformedValues(t *testing.T) {
	t.Setenv("N", "many")
	t.Setenv("B", "yes-please")
	t.Setenv("D", "30")    // no unit
	t.Setenv("NEG", "-5s") // negative

	var e env
	e.optionalInt("N", 0)
	e.optionalBool("B", false)
	e.optionalDuration("D", 0)
	e.optionalDuration("NEG", 0)

	err := e.err()
	if err == nil {
		t.Fatal("err() = nil, want four problems")
	}
	for _, want := range []string{"N must be an integer", "B must be a boolean", "D must be a duration", "NEG must not be negative"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}

func TestMalformedValueStillReturnsTheDefault(t *testing.T) {
	t.Setenv("N", "many")

	var e env
	// The caller is expected to check err(), but returning the default rather
	// than a zero value keeps the failure path from cascading into confusing
	// secondary problems while the rest of the config is being collected.
	if got := e.optionalInt("N", 7); got != 7 {
		t.Errorf("optionalInt on a bad value = %d, want the default 7", got)
	}
}
