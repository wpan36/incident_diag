package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// env reads values from the process environment, collecting every problem it
// finds instead of stopping at the first one. An operator starting a service
// with three variables wrong should be told about all three, not made to fix
// them one restart at a time.
//
// The zero value is ready to use.
type env struct {
	problems []string
}

// err returns all collected problems as a single error, or nil if there were
// none.
func (e *env) err() error {
	if len(e.problems) == 0 {
		return nil
	}
	if len(e.problems) == 1 {
		return fmt.Errorf("invalid configuration: %s", e.problems[0])
	}
	return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(e.problems, "\n  - "))
}

func (e *env) fail(format string, args ...any) {
	e.problems = append(e.problems, fmt.Sprintf(format, args...))
}

// lookup returns the trimmed value of key and whether it was set to anything
// other than whitespace. Treating "  " as unset avoids the common .env mistake
// of leaving a key with an empty value and getting a confusing failure later.
func lookup(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	return v, v != ""
}

// requiredString records a problem if key is unset or empty.
func (e *env) requiredString(key string) string {
	v, ok := lookup(key)
	if !ok {
		e.fail("%s is required but not set", key)
		return ""
	}
	return v
}

// optionalString returns the value of key, or def if it is unset or empty.
func (e *env) optionalString(key, def string) string {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	return v
}

// optionalInt returns the value of key parsed as an integer, or def if unset.
func (e *env) optionalInt(key string, def int) int {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.fail("%s must be an integer, got %q", key, v)
		return def
	}
	return n
}

// optionalInt64 returns the value of key parsed as a 64-bit integer, or def if
// unset. It exists for byte counts, which are not bounded by int on every
// platform this could be built for.
func (e *env) optionalInt64(key string, def int64) int64 {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		e.fail("%s must be an integer, got %q", key, v)
		return def
	}
	return n
}

// optionalBool returns the value of key parsed as a boolean, or def if unset.
// It accepts the same spellings as strconv.ParseBool (1, t, true, 0, f, false,
// and their capitalizations).
func (e *env) optionalBool(key string, def bool) bool {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.fail("%s must be a boolean, got %q", key, v)
		return def
	}
	return b
}

// optionalDuration returns the value of key parsed as a Go duration ("30s",
// "5m"), or def if unset. Negative durations are rejected: every duration in
// this project is a timeout or an interval, and neither is meaningful negative.
func (e *env) optionalDuration(key string, def time.Duration) time.Duration {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.fail("%s must be a duration such as 30s or 5m, got %q", key, v)
		return def
	}
	if d < 0 {
		e.fail("%s must not be negative, got %q", key, v)
		return def
	}
	return d
}

// oneOf returns the value of key if it is one of allowed, or def if unset.
func (e *env) oneOf(key, def string, allowed ...string) string {
	v, ok := lookup(key)
	if !ok {
		return def
	}
	for _, a := range allowed {
		if strings.EqualFold(v, a) {
			return strings.ToLower(v)
		}
	}
	e.fail("%s must be one of %s, got %q", key, strings.Join(allowed, ", "), v)
	return def
}
