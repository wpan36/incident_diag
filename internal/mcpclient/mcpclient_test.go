package mcpclient

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/opsmcp"
)

// connected starts a real ops-mcp over HTTP and connects to it.
//
// A real server rather than a hand-rolled fake: the thing worth testing is that
// the two halves of the meta envelope agree, and a fake would be written from
// the same reading of the spec that the client was.
func connected(t *testing.T, cfg config.OpsMCP) *Client {
	t.Helper()

	srv := httptest.NewServer(opsmcp.New(cfg, log.Discard()).Handler())
	t.Cleanup(srv.Close)

	c, err := Connect(context.Background(), srv.URL+"/mcp", 5*time.Second, log.Discard())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// logDir writes one service log and returns its directory.
func logDir(t *testing.T, service, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, service+".log"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the log fixture: %v", err)
	}
	return dir
}

func baseConfig(t *testing.T) config.OpsMCP {
	t.Helper()
	return config.OpsMCP{
		PrometheusURL:     "http://127.0.0.1:1/unused",
		ProbeTargets:      map[string]string{"payment-service": "http://127.0.0.1:1"},
		LogRoot:           logDir(t, "payment-service", `{"time":"2026-09-21T10:00:00Z","level":"ERROR","msg":"pool exhausted","in_use":20}`+"\n"),
		LogServices:       []string{"payment-service"},
		PrometheusTimeout: time.Second,
		MaxRange:          6 * time.Hour,
		MinStep:           15 * time.Second,
		MaxSeries:         50,
		ProbeTimeout:      500 * time.Millisecond,
		ProbeBodyBytes:    2 << 10,
		MaxLogLines:       1000,
	}
}

func call(t *testing.T, c *Client, name string, args map[string]any) Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshalling arguments: %v", err)
	}
	res, err := c.Call(context.Background(), name, raw)
	if err != nil {
		t.Fatalf("Call(%s): %v", name, err)
	}
	return res
}

func TestConnectDiscoversTheTools(t *testing.T) {
	c := connected(t, baseConfig(t))

	names := map[string]bool{}
	for _, tool := range c.Tools() {
		names[tool.Name] = true
		if tool.Description == "" {
			t.Errorf("tool %s has no description; the model reads it", tool.Name)
		}
		// The schema goes to the model unchanged, so it has to be JSON that
		// describes an object with properties.
		var schema struct {
			Type       string         `json:"type"`
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("tool %s has an unparseable schema: %v", tool.Name, err)
		}
		if schema.Type != "object" || len(schema.Properties) == 0 {
			t.Errorf("tool %s schema = %s", tool.Name, tool.InputSchema)
		}
	}
	for _, want := range []string{"prometheus_query", "http_probe", "read_service_logs"} {
		if !names[want] {
			t.Errorf("the server did not expose %s", want)
		}
	}
}

func TestUnknownServiceIsRefusedNotAnError(t *testing.T) {
	c := connected(t, baseConfig(t))

	for _, tool := range []string{"http_probe", "read_service_logs"} {
		res := call(t, c, tool, map[string]any{"service": "billing-service"})

		// A refusal is a normal result: an MCP error would be
		// indistinguishable from the server being broken, and the model would
		// spend one of its bounded tool calls learning nothing.
		if res.Status != StatusOK {
			t.Errorf("%s: status = %s, want OK", tool, res.Status)
		}
		if !res.Refused {
			t.Errorf("%s: refused = false", tool)
		}
		// The valid names are listed so the next attempt can be right rather
		// than another guess.
		if !strings.Contains(res.Text, "payment-service") {
			t.Errorf("%s: the refusal does not name the valid services: %q", tool, res.Text)
		}
	}
}

func TestReadServiceLogsRoundTrip(t *testing.T) {
	c := connected(t, baseConfig(t))

	res := call(t, c, "read_service_logs", map[string]any{
		"service": "payment-service", "min_level": "WARN",
	})
	if res.Status != StatusOK || res.Refused {
		t.Fatalf("status=%s refused=%v note=%q", res.Status, res.Refused, res.Note)
	}
	if !strings.Contains(res.Text, "pool exhausted") || !strings.Contains(res.Text, "in_use=20") {
		t.Errorf("text = %q", res.Text)
	}
	// Read from the envelope, not counted off the prose.
	if res.OriginalBytes != len(res.Text) {
		t.Errorf("original_bytes = %d, text is %d", res.OriginalBytes, len(res.Text))
	}
	if res.Truncated {
		t.Error("a short result was reported as truncated")
	}
}

func TestBadArgumentsAreRefused(t *testing.T) {
	c := connected(t, baseConfig(t))

	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"read_service_logs", map[string]any{"service": "payment-service", "min_level": "LOUD"}, "min_level"},
		{"read_service_logs", map[string]any{"service": "payment-service", "since": "-5m"}, "RFC 3339"},
		{"read_service_logs", map[string]any{"service": "payment-service", "limit": 99999}, "limit"},
		{"http_probe", map[string]any{"service": "payment-service", "path": "//evil.example/x"}, "two slashes"},
		{"http_probe", map[string]any{"service": "payment-service", "path": "http://evil.example/x"}, "single slash"},
		{"prometheus_query", map[string]any{"query": "up", "start": "2026-09-21T10:00:00Z"}, "together"},
		{"prometheus_query", map[string]any{"query": ""}, "required"},
	}
	for _, c2 := range cases {
		res := call(t, c, c2.tool, c2.args)
		if !res.Refused || res.Status != StatusOK {
			t.Errorf("%s %v: refused=%v status=%s", c2.tool, c2.args, res.Refused, res.Status)
			continue
		}
		if !strings.Contains(res.Text, c2.want) {
			t.Errorf("%s %v: text = %q, want it to mention %q", c2.tool, c2.args, res.Text, c2.want)
		}
		// Whichever rule fired, nothing that names another host may come back
		// as something the agent could then probe.
		if strings.Contains(res.Text, "evil.example/x") && !res.Refused {
			t.Errorf("%s: %q was not refused", c2.tool, res.Text)
		}
	}
}

func TestUnreachableDependencyIsAnError(t *testing.T) {
	c := connected(t, baseConfig(t))

	// Port 1 refuses connections, so these exercise the path where the model
	// cannot fix the problem and must not keep retrying.
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"prometheus_query", map[string]any{"query": "up"}},
		{"http_probe", map[string]any{"service": "payment-service", "path": "/health"}},
	} {
		res := call(t, c, tc.tool, tc.args)
		if res.Status != StatusError && res.Status != StatusTimeout {
			t.Errorf("%s: status = %s, want ERROR or TIMEOUT", tc.tool, res.Status)
		}
		if res.Refused {
			t.Errorf("%s: an unreachable dependency was reported as refused", tc.tool)
		}
	}
}

func TestMissingLogFileIsAnError(t *testing.T) {
	cfg := baseConfig(t)
	cfg.LogServices = append(cfg.LogServices, "checkout-service") // configured, but no file
	c := connected(t, cfg)

	res := call(t, c, "read_service_logs", map[string]any{"service": "checkout-service"})
	if res.Status != StatusError {
		t.Errorf("status = %s, want ERROR", res.Status)
	}
	if res.Refused {
		t.Error("a missing file was reported as something the model could fix")
	}
}

func TestCallingAnUnknownToolIsAnError(t *testing.T) {
	c := connected(t, baseConfig(t))

	// A tool the server does not have is a protocol error, not a result: it
	// means the caller is broken, not that the investigation learned something.
	if _, err := c.Call(context.Background(), "rm_minus_rf", json.RawMessage(`{}`)); err == nil {
		t.Fatal("calling an unknown tool succeeded")
	}
}
