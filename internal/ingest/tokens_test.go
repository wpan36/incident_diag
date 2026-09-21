package ingest

import (
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		// Four ASCII runes are a quarter of a token each.
		{"ascii", "word", 1},
		{"ascii rounds up", "words", 2},
		// Twelve ideographs at 1.5 runes per token is eight.
		{"cjk", strings.Repeat("支", 12), 8},
		// The catch-all rule is what keeps this function total. Cyrillic,
		// accented Latin and emoji are none of the two alphabets a rule written
		// for "ASCII or CJK" would cover, and they must still count.
		{"cyrillic", "отказ", 2},
		{"accented latin", "café", 1},
		{"emoji", "😀😀😀😀", 1},
		// A mixed line is the realistic case: 8 CJK runes plus 8 ASCII is
		// 5.33 + 2 = 7.33.
		{"mixed", strings.Repeat("延迟", 4) + strings.Repeat("ab", 4), 8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EstimateTokens(c.in); got != c.want {
				t.Errorf("EstimateTokens(%q) = %d, want %d", c.in, got, c.want)
			}
		})
	}
}

// The estimate is computed over the embedded text, prefix included. A deep
// heading path is sixty or eighty characters that are part of what gets sent,
// so packing against the bare body would push chunks over the budget by
// construction.
func TestTheEstimateIncludesTheHeadingPath(t *testing.T) {
	body := "Raise the pool size to 50."
	p := "Payment Service > Latency > Connection pool"
	if EstimateTokens(embedded(p, body)) <= EstimateTokens(body) {
		t.Error("the heading path must count towards the estimate")
	}
}
