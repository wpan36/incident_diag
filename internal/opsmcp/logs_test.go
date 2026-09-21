package opsmcp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// line renders one log record the way internal/log writes it.
func line(at, level, msg string, extra ...string) string {
	s := fmt.Sprintf(`{"time":%q,"level":%q,"msg":%q`, at, level, msg)
	for i := 0; i+1 < len(extra); i += 2 {
		s += fmt.Sprintf(`,%q:%q`, extra[i], extra[i+1])
	}
	return s + "}"
}

func scan(t *testing.T, body string, f logFilter) ([]record, int, int) {
	t.Helper()
	if f.limit == 0 {
		f.limit = defaultLogLimit
	}
	recs, matched, unparseable, err := scanLog(context.Background(), strings.NewReader(body), f)
	if err != nil {
		t.Fatalf("scanLog: %v", err)
	}
	return recs, matched, unparseable
}

const (
	t1 = "2026-09-21T10:00:00Z"
	t2 = "2026-09-21T10:05:00Z"
	t3 = "2026-09-21T10:10:00Z"
)

func TestScanLogFiltersByLevel(t *testing.T) {
	body := strings.Join([]string{
		line(t1, "DEBUG", "noise"),
		line(t2, "INFO", "ordinary"),
		line(t3, "ERROR", "the interesting one"),
	}, "\n")

	recs, matched, _ := scan(t, body, logFilter{minLevel: levels["WARN"]})
	if matched != 1 || len(recs) != 1 || recs[0].Msg != "the interesting one" {
		t.Errorf("matched=%d recs=%+v", matched, recs)
	}
}

func TestScanLogFiltersByTimeWindow(t *testing.T) {
	body := strings.Join([]string{
		line(t1, "INFO", "before"),
		line(t2, "INFO", "inside"),
		line(t3, "INFO", "after"),
	}, "\n")

	since, _ := time.Parse(time.RFC3339, t2)
	until, _ := time.Parse(time.RFC3339, t3)
	recs, matched, _ := scan(t, body, logFilter{minLevel: levels["DEBUG"], since: since, until: until})

	// since is inclusive and until is exclusive, so a window can be tiled
	// without a record appearing in two of them.
	if matched != 1 || recs[0].Msg != "inside" {
		t.Errorf("matched=%d recs=%+v", matched, recs)
	}
}

func TestScanLogMatchesTheRawLineNotTheRenderedOne(t *testing.T) {
	body := line(t1, "INFO", "starting", "request_id", "01ABCDEF")

	// The attribute is searchable even though the renderer might not print it,
	// and the match is case-insensitive.
	recs, matched, _ := scan(t, body, logFilter{minLevel: levels["DEBUG"], contains: "01abcdef"})
	if matched != 1 || len(recs) != 1 {
		t.Errorf("matched=%d recs=%+v", matched, recs)
	}
}

func TestScanLogKeepsTheTail(t *testing.T) {
	var lines []string
	for i := range 10 {
		lines = append(lines, line(t1, "INFO", fmt.Sprintf("msg-%d", i)))
	}

	recs, matched, _ := scan(t, strings.Join(lines, "\n"), logFilter{minLevel: levels["DEBUG"], limit: 3})
	if matched != 10 {
		t.Errorf("matched = %d, want all 10 counted", matched)
	}
	// The last three, oldest first: debugging wants the most recent records,
	// and matched still reports how many there were.
	if len(recs) != 3 || recs[0].Msg != "msg-7" || recs[2].Msg != "msg-9" {
		t.Errorf("kept %+v", recs)
	}
}

func TestScanLogCountsUnparseableLines(t *testing.T) {
	body := strings.Join([]string{
		line(t1, "INFO", "good"),
		"not json at all",
		`{"time":"nonsense","level":"INFO","msg":"bad time"}`,
		`{"level":"INFO","msg":"no time"}`,
		`{"time":"` + t2 + `","level":"LOUD","msg":"unknown level"}`,
		`{"time":"` + t2 + `","level":"INFO"}`,
		line(t3, "INFO", "also good"),
	}, "\n")

	recs, matched, unparseable := scan(t, body, logFilter{minLevel: levels["DEBUG"]})
	if matched != 2 || len(recs) != 2 {
		t.Errorf("matched=%d recs=%+v", matched, recs)
	}
	// Five bad lines — not JSON, unparseable time, no time, unknown level, no
	// msg — counted rather than dropped. A line missing a timestamp would
	// silently pass or fail the time filter, and a wrong answer about when
	// something happened is worse than a counted line.
	if unparseable != 5 {
		t.Errorf("unparseable = %d, want 5", unparseable)
	}
}

func TestScanLogStopsOnCancellation(t *testing.T) {
	var lines []string
	for i := range 5000 {
		lines = append(lines, line(t1, "INFO", fmt.Sprintf("msg-%d", i)))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, _, err := scanLog(ctx, strings.NewReader(strings.Join(lines, "\n")),
		logFilter{minLevel: levels["DEBUG"], limit: defaultLogLimit})
	if err == nil {
		t.Fatal("scanLog ignored a cancelled context")
	}
}

func TestRenderRecordsReportsWhatWasNotShown(t *testing.T) {
	at, _ := time.Parse(time.RFC3339, t1)
	recs := []record{{At: at, Level: "ERROR", Msg: "pool exhausted",
		Attrs: map[string]any{"service": "payment-service", "in_use": float64(20)}}}

	got := renderRecords(recs, "payment-service", 40, 3)
	for _, want := range []string{
		"40 matching records", "showing the last 1", "3 unparseable lines skipped",
		"2026-09-21T10:00:00Z ERROR pool exhausted",
		"in_use=20", "service=payment-service",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("render is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderRecordsOnNoMatch(t *testing.T) {
	got := renderRecords(nil, "payment-service", 0, 0)
	if !strings.Contains(got, "no records matched") {
		t.Errorf("render = %q", got)
	}
}

func TestScanLogCountsAnOverLongLineAndKeepsReading(t *testing.T) {
	// A Scanner cannot continue past a line longer than its buffer, so one
	// corrupt line used to cost the whole file — and with no rotation, that
	// service's log would have stayed unreadable for good.
	body := strings.Join([]string{
		line(t1, "INFO", "before"),
		`{"time":"` + t2 + `","level":"INFO","msg":"` + strings.Repeat("x", maxLogLineBytes+1) + `"}`,
		line(t3, "INFO", "after"),
	}, "\n")

	recs, matched, unparseable := scan(t, body, logFilter{minLevel: levels["DEBUG"]})
	if matched != 2 || len(recs) != 2 || recs[0].Msg != "before" || recs[1].Msg != "after" {
		t.Errorf("matched=%d recs=%+v", matched, recs)
	}
	if unparseable != 1 {
		t.Errorf("unparseable = %d, want 1", unparseable)
	}
}

func TestScanLogReadsALongButLegalLine(t *testing.T) {
	// Longer than the read buffer, shorter than the bound: a big stack trace
	// has to survive in one piece.
	msg := strings.Repeat("y", readBufferBytes*2)
	body := line(t1, "ERROR", msg) + "\n"

	recs, matched, unparseable := scan(t, body, logFilter{minLevel: levels["DEBUG"]})
	if matched != 1 || unparseable != 0 || len(recs) != 1 || recs[0].Msg != msg {
		t.Errorf("matched=%d unparseable=%d recs=%d", matched, unparseable, len(recs))
	}
}

func TestScanLogReadsALastLineWithNoNewline(t *testing.T) {
	body := line(t1, "INFO", "first") + "\n" + line(t2, "INFO", "unterminated")

	recs, matched, _ := scan(t, body, logFilter{minLevel: levels["DEBUG"]})
	if matched != 2 || len(recs) != 2 || recs[1].Msg != "unterminated" {
		t.Errorf("matched=%d recs=%+v", matched, recs)
	}
}
