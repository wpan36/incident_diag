package id

import (
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewIsSortableWithinAMillisecond(t *testing.T) {
	// Everything generated here lands in the same millisecond, which is
	// exactly the case a non-monotonic source would get wrong.
	const n = 200
	ids := make([]string, n)
	at := time.Date(2026, 9, 20, 10, 11, 12, 0, time.UTC)
	for i := range ids {
		ids[i] = NewAt(at)
	}

	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("ids are not in lexical order at %d: %q vs %q", i, ids[i], sorted[i])
		}
	}
}

func TestNewIsUniqueUnderConcurrency(t *testing.T) {
	const goroutines, each = 16, 100

	var wg sync.WaitGroup
	results := make([][]string, goroutines)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := make([]string, each)
			for i := range out {
				out[i] = New()
			}
			results[g] = out
		}()
	}
	wg.Wait()

	seen := make(map[string]bool, goroutines*each)
	for _, batch := range results {
		for _, v := range batch {
			if seen[v] {
				t.Fatalf("duplicate id %q", v)
			}
			seen[v] = true
		}
	}
}

func TestNewHasTheWidthTheSchemaExpects(t *testing.T) {
	// The columns are CHAR(26). An id of any other length would be silently
	// padded or rejected by MySQL depending on its mode.
	if got := len(New()); got != 26 {
		t.Fatalf("len = %d, want 26", got)
	}
}

func TestValid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"generated", New(), true},
		{"empty", "", false},
		{"too short", "01JBQ8K3M7VXFZ2N9WQYRT4HC", false},
		{"too long", "01JBQ8K3M7VXFZ2N9WQYRT4HCDE", false},
		{"excluded letter", "01JBQ8K3M7VXFZ2N9WQYRT4HCI", false},
		{"lowercase is accepted", strings.ToLower(New()), true},
		{"overflows the timestamp", "ZZZZZZZZZZZZZZZZZZZZZZZZZZ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Valid(tt.in); got != tt.want {
				t.Fatalf("Valid(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}
