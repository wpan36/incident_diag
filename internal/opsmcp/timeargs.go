package opsmcp

import (
	"fmt"
	"time"
)

// timeLayout is the only timestamp format any tool accepts.
//
// Relative times ("5m", "now-1h") are deliberately not accepted. The agent has
// the incident's timestamps, and one format means one parser and no ambiguity
// about whose clock "now" belongs to — the model's idea of it, the server's, or
// the log line's.
const timeLayout = time.RFC3339

// parseTime reads an RFC 3339 timestamp, normalized to UTC.
func parseTime(param, raw string) (time.Time, error) {
	t, err := time.Parse(timeLayout, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"%s must be an RFC 3339 timestamp such as 2026-09-21T10:04:05Z, got %q", param, raw)
	}
	return t.UTC(), nil
}

// parseWindow reads an optional start/end pair.
//
// Giving exactly one of them is refused rather than guessed. "From then until
// now" and "from the beginning until then" are both plausible readings, and a
// server that picks one silently answers a question the agent did not ask.
func parseWindow(startRaw, endRaw string) (start, end time.Time, ranged bool, err error) {
	switch {
	case startRaw == "" && endRaw == "":
		return time.Time{}, time.Time{}, false, nil
	case startRaw == "" || endRaw == "":
		return time.Time{}, time.Time{}, false, fmt.Errorf(
			"start and end must be given together or not at all")
	}

	if start, err = parseTime("start", startRaw); err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if end, err = parseTime("end", endRaw); err != nil {
		return time.Time{}, time.Time{}, false, err
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, false, fmt.Errorf("end must be after start")
	}
	return start, end, true, nil
}
