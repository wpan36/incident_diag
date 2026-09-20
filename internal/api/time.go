package api

import (
	"fmt"
	"strconv"
	"time"
)

// TimeLayout is the one timestamp format this API emits: RFC 3339 in UTC with
// fixed microsecond precision, "2026-09-20T10:11:12.345678Z".
//
// Go's default marshalling of a time.Time is RFC 3339 *Nano*, which trims
// trailing zeros, so the same instant would serialize to a different number of
// digits depending on its value — 10:11:12Z one moment and 10:11:12.345678Z the
// next. A client parsing those has to handle both, and a test asserting on one
// passes or fails by luck.
//
// Six digits is what DATETIME(6) stores, so nothing is lost by fixing it here.
const TimeLayout = "2006-01-02T15:04:05.000000Z07:00"

// Time is a time.Time that marshals through TimeLayout.
//
// The formatting is a property of the type rather than of each call site, so a
// response struct cannot be given a bare time.Time by accident: it would not
// compile against these field types.
type Time time.Time

// NewTime wraps t for a response.
func NewTime(t time.Time) Time { return Time(t) }

// NullTime wraps a nullable timestamp, preserving the difference between "no
// value" and the zero time.
func NullTime(t *time.Time) *Time {
	if t == nil {
		return nil
	}
	v := Time(*t)
	return &v
}

// MarshalJSON renders the instant in UTC. Converting rather than assuming keeps
// the output correct even if a value ever reaches the API in another zone.
func (t Time) MarshalJSON() ([]byte, error) {
	return strconv.AppendQuote(nil, t.String()), nil
}

func (t Time) String() string { return time.Time(t).UTC().Format(TimeLayout) }

// UnmarshalJSON accepts any RFC 3339 timestamp, which includes everything
// MarshalJSON produces.
//
// No endpoint takes a timestamp from a client. It exists so the type
// round-trips: a type that can be written but not read back is a type a test —
// or a generated Go client — cannot decode a response into, and the asymmetry
// is the kind that gets noticed much later.
func (t *Time) UnmarshalJSON(b []byte) error {
	s, err := strconv.Unquote(string(b))
	if err != nil {
		// null is the wire form of an absent timestamp; the caller holds it in
		// a *Time and gets nil, so there is nothing to parse here.
		if string(b) == "null" {
			return nil
		}
		return fmt.Errorf("timestamp must be a JSON string: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return fmt.Errorf("timestamp %q is not RFC 3339: %w", s, err)
	}
	*t = Time(parsed.UTC())
	return nil
}
