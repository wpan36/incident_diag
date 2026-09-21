package log

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// record decodes the single JSON log line written to buf.
func record(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	line := strings.TrimSpace(buf.String())
	if line == "" {
		t.Fatal("nothing was logged")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("expected one log line, got:\n%s", line)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("log line is not JSON: %v\n%s", err, line)
	}
	return m
}

func TestIdentifiersFlowFromContext(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo, "test-service")

	ctx := WithRunID(WithRequestID(context.Background(), "req-1"), "run-9")
	logger.InfoContext(ctx, "investigating")

	got := record(t, &buf)
	if got[KeyRequestID] != "req-1" {
		t.Errorf("%s = %v, want req-1", KeyRequestID, got[KeyRequestID])
	}
	if got[KeyRunID] != "run-9" {
		t.Errorf("%s = %v, want run-9", KeyRunID, got[KeyRunID])
	}
	if got["msg"] != "investigating" {
		t.Errorf("msg = %v", got["msg"])
	}
}

func TestAbsentIdentifiersAreOmitted(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo, "test-service")

	logger.InfoContext(context.Background(), "starting")

	got := record(t, &buf)
	// An empty request_id on every startup line is noise that makes real ones
	// harder to grep for.
	if _, ok := got[KeyRequestID]; ok {
		t.Errorf("%s present with no identifier in context", KeyRequestID)
	}
	if _, ok := got[KeyRunID]; ok {
		t.Errorf("%s present with no identifier in context", KeyRunID)
	}
}

func TestDerivedLoggerKeepsContextIdentifiers(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo, "test-service")

	// The regression this guards: if WithAttrs did not rewrap the handler, the
	// derived logger would fall back to the plain JSON handler and quietly stop
	// adding identifiers, which is exactly the kind of gap nobody notices until
	// they need the logs.
	derived := logger.With("component", "agent").WithGroup("detail")

	ctx := WithRequestID(context.Background(), "req-2")
	derived.InfoContext(ctx, "step complete", "step", 3)

	got := record(t, &buf)
	if got[KeyRequestID] != "req-2" {
		t.Errorf("%s = %v, want req-2 on a derived logger", KeyRequestID, got[KeyRequestID])
	}
	if got["component"] != "agent" {
		t.Errorf("component = %v, want agent", got["component"])
	}
	detail, ok := got["detail"].(map[string]any)
	if !ok {
		t.Fatalf("detail group missing: %v", got)
	}
	if detail["step"] != float64(3) {
		t.Errorf("detail.step = %v, want 3", detail["step"])
	}
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelWarn, "test-service")

	logger.InfoContext(context.Background(), "should not appear")
	if buf.Len() != 0 {
		t.Errorf("info was written at warn level: %s", buf.String())
	}

	logger.WarnContext(context.Background(), "should appear")
	if got := record(t, &buf); got["msg"] != "should appear" {
		t.Errorf("msg = %v", got["msg"])
	}
}

func TestAccessorsOnAnEmptyContext(t *testing.T) {
	if got := RequestID(context.Background()); got != "" {
		t.Errorf("RequestID = %q, want empty", got)
	}
	if got := RunID(context.Background()); got != "" {
		t.Errorf("RunID = %q, want empty", got)
	}
}

func TestDiscardWritesNothing(t *testing.T) {
	// Discard must still be safe to call with a context, which is how every
	// other logger in the project is used.
	Discard().InfoContext(WithRunID(context.Background(), "run-1"), "ignored")
}

func TestDerivedLoggersDoNotShareAttributes(t *testing.T) {
	// Two loggers derived from the same parent must not leak into each other.
	// The bug this guards against is appending to a shared slice: the second
	// derivation overwrites the first one's entry in place.
	var buf bytes.Buffer
	parent := New(&buf, slog.LevelInfo, "test-service").With("shared", "yes")

	a := parent.With("which", "a")
	b := parent.With("which", "b")

	ctx := WithRequestID(context.Background(), "req-3")

	a.InfoContext(ctx, "from a")
	got := record(t, &buf)
	if got["which"] != "a" {
		t.Errorf("logger a logged which=%v", got["which"])
	}
	buf.Reset()

	b.InfoContext(ctx, "from b")
	got = record(t, &buf)
	if got["which"] != "b" {
		t.Errorf("logger b logged which=%v", got["which"])
	}
	if got["shared"] != "yes" {
		t.Errorf("shared attribute lost: %v", got)
	}
	if got[KeyRequestID] != "req-3" {
		t.Errorf("%s = %v", KeyRequestID, got[KeyRequestID])
	}
}

func TestGroupedLoggerKeepsIdentifiersTopLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo, "test-service").WithGroup("tool").With("name", "prometheus_query")

	ctx := WithRunID(context.Background(), "run-7")
	logger.InfoContext(ctx, "tool finished", "duration_ms", 12)

	got := record(t, &buf)
	// The identifier belongs at the top level. Nested under "tool" it would be
	// invisible to every query written against run_id.
	if got[KeyRunID] != "run-7" {
		t.Errorf("%s = %v at top level, want run-7; full record: %v", KeyRunID, got[KeyRunID], got)
	}
	group, ok := got["tool"].(map[string]any)
	if !ok {
		t.Fatalf("tool group missing: %v", got)
	}
	if group["name"] != "prometheus_query" {
		t.Errorf("tool.name = %v", group["name"])
	}
	if group["duration_ms"] != float64(12) {
		t.Errorf("tool.duration_ms = %v", group["duration_ms"])
	}
	if _, nested := group[KeyRunID]; nested {
		t.Errorf("%s was nested inside the group", KeyRunID)
	}
}

func TestEnabledIsDelegated(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelWarn, "test-service")
	ctx := context.Background()

	if logger.Enabled(ctx, slog.LevelInfo) {
		t.Error("Enabled(info) = true at warn level")
	}
	if !logger.Enabled(ctx, slog.LevelError) {
		t.Error("Enabled(error) = false at warn level")
	}
	// Enabled must keep working through derivation, since slog consults it
	// before building a record.
	if logger.With("k", "v").WithGroup("g").Enabled(ctx, slog.LevelInfo) {
		t.Error("Enabled(info) = true on a derived logger at warn level")
	}
}

func TestServiceIsOnEveryRecord(t *testing.T) {
	var buf bytes.Buffer
	// The attribute has to survive derivation and grouping, because that is
	// how it would silently disappear: read_service_logs identifies a file by
	// service, but anything reading several files together needs the field.
	logger := New(&buf, slog.LevelInfo, "payment-service").With("component", "pool").WithGroup("detail")

	logger.InfoContext(context.Background(), "charge complete")

	got := record(t, &buf)
	if got[KeyService] != "payment-service" {
		t.Errorf("%s = %v, want payment-service; full record: %v", KeyService, got[KeyService], got)
	}
}

func TestEmptyServiceIsOmitted(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo, "").InfoContext(context.Background(), "hello")

	if _, ok := record(t, &buf)[KeyService]; ok {
		t.Errorf("%s present for an empty service name", KeyService)
	}
}

func TestTimestampIsUTCWithMillisecondPrecision(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo, "api").InfoContext(context.Background(), "hello")

	raw, ok := record(t, &buf)[slog.TimeKey].(string)
	if !ok {
		t.Fatalf("time is not a string: %v", buf.String())
	}
	// Zulu, not +08:00: a time-window filter across services whose containers
	// disagree about the zone is silently wrong.
	if !strings.HasSuffix(raw, "Z") {
		t.Errorf("time = %q, want a UTC timestamp ending in Z", raw)
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("time %q is not RFC 3339: %v", raw, err)
	}
	// RFC3339Nano drops trailing zeros, so the assertion is on precision
	// rather than on the number of digits.
	if ts.Nanosecond()%int(time.Millisecond) != 0 {
		t.Errorf("time = %q, want millisecond precision", raw)
	}
	if d := time.Since(ts); d < 0 || d > time.Minute {
		t.Errorf("time = %q is %v away from now", raw, d)
	}
}
