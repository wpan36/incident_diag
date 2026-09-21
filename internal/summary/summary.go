// Package summary caps text at a byte limit on a rune boundary.
//
// It exists because two packages have to agree on the cap and on how it is
// applied: internal/store writes the capped text into MEDIUMTEXT columns, and
// ops-mcp applies the same cap to a tool result before the text ever leaves the
// server. Two implementations that must produce identical results are one
// implementation with a latent disagreement.
package summary

import "unicode/utf8"

// LimitBytes is the cap on an observation or tool result summary, fixed by the
// data model spec.
const LimitBytes = 8 << 10 // 8 KiB

// Cap truncates s to LimitBytes bytes of UTF-8, reporting the original length
// and whether it was cut.
//
// The three results are returned together so that no caller can record one
// without the others — a stored summary whose byte count describes a different
// string is worse than no byte count at all.
func Cap(s string) (text string, originalBytes int, truncated bool) {
	return CapAt(s, LimitBytes)
}

// CapAt is Cap at a caller's limit, for the smaller budgets inside a result —
// an HTTP probe's body, say, which is capped long before the whole result is.
//
// The cut falls on a rune boundary at or below the limit, never inside a
// multi-byte sequence. Slicing at a fixed byte offset would produce invalid
// UTF-8, which a utf8mb4 column rejects or silently mangles, and tool output
// contains non-ASCII often enough for that to be a matter of when rather than
// whether.
func CapAt(s string, limit int) (text string, originalBytes int, truncated bool) {
	if limit < 0 {
		limit = 0
	}
	if len(s) <= limit {
		return s, len(s), false
	}
	cut := limit
	// s[cut] is the first byte that will be dropped. While it is a continuation
	// byte the cut is inside a rune, so step back.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], len(s), true
}
