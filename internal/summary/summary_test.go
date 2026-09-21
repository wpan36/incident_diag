package summary

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCapLeavesShortTextAlone(t *testing.T) {
	text, n, cut := Cap("hello")
	if text != "hello" || n != 5 || cut {
		t.Errorf("Cap = %q, %d, %v", text, n, cut)
	}
}

func TestCapAtExactlyTheLimit(t *testing.T) {
	s := strings.Repeat("a", LimitBytes)
	text, n, cut := Cap(s)
	if cut || len(text) != LimitBytes || n != LimitBytes {
		t.Errorf("text at exactly the limit was cut: len=%d truncated=%v", len(text), cut)
	}
}

func TestCapReportsTheOriginalLength(t *testing.T) {
	s := strings.Repeat("a", LimitBytes+100)
	text, n, cut := Cap(s)
	if !cut {
		t.Fatal("over-limit text was not cut")
	}
	// The original length, not the length of what survived: the point of the
	// field is to say how much was lost.
	if n != LimitBytes+100 {
		t.Errorf("originalBytes = %d, want %d", n, LimitBytes+100)
	}
	if len(text) != LimitBytes {
		t.Errorf("kept %d bytes, want %d", len(text), LimitBytes)
	}
}

func TestCapNeverSplitsARune(t *testing.T) {
	// Pad with ASCII so the cap lands inside the multi-byte rune that follows.
	// A naive slice here produces invalid UTF-8, which is what a utf8mb4 column
	// rejects.
	for pad := LimitBytes - 2; pad < LimitBytes; pad++ {
		s := strings.Repeat("a", pad) + strings.Repeat("世", 10)
		text, _, cut := Cap(s)
		if !cut {
			t.Fatalf("pad=%d: not cut", pad)
		}
		if !utf8.ValidString(text) {
			t.Errorf("pad=%d: cut produced invalid UTF-8", pad)
		}
		if len(text) > LimitBytes {
			t.Errorf("pad=%d: kept %d bytes, over the cap", pad, len(text))
		}
	}
}

func TestCapAtASmallerLimit(t *testing.T) {
	// The probe body cap is a budget inside a result, cut long before the whole
	// result reaches the 8 KiB limit.
	text, n, cut := CapAt("hello world", 5)
	if text != "hello" || n != 11 || !cut {
		t.Errorf("CapAt = %q, %d, %v", text, n, cut)
	}
	if text, _, cut := CapAt("世界", 4); cut && !utf8.ValidString(text) {
		t.Errorf("CapAt produced invalid UTF-8: %q", text)
	}
	// A limit with no room at all still returns something valid.
	if text, _, cut := CapAt("世", 1); !cut || text != "" {
		t.Errorf("CapAt(_, 1) = %q, %v", text, cut)
	}
}
