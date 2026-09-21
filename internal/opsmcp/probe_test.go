package opsmcp

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/log"
)

const probeBase = "http://payment-service:8080"

// probeBodyBytes is the cap these tests configure, small enough to be readable
// in a failure message.
const probeBodyBytes = 64

func TestJoinProbePathAccepts(t *testing.T) {
	cases := map[string]string{
		"/":            probeBase + "/",
		"/health":      probeBase + "/health",
		"/a/b/c":       probeBase + "/a/b/c",
		"/q?x=1&y=2":   probeBase + "/q?x=1&y=2",
		"/has%20space": probeBase + "/has%20space",
	}
	for path, want := range cases {
		got, err := joinProbePath(probeBase, path)
		if err != nil {
			t.Errorf("joinProbePath(%q): %v", path, err)
			continue
		}
		if got != want {
			t.Errorf("joinProbePath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestJoinProbePathRefusesAnythingThatLeavesTheHost(t *testing.T) {
	// The first two are the reason this function exists: both are valid URL
	// references, and ResolveReference sends both to another host.
	cases := []string{
		"//evil.example/x",
		"http://evil.example/x",
		"https://evil.example/x",
		"health",
		"",
		"../../etc/passwd",
		"\\\\evil.example\\x",
	}
	for _, path := range cases {
		got, err := joinProbePath(probeBase, path)
		if err == nil {
			t.Errorf("joinProbePath(%q) = %q, want a refusal", path, got)
			continue
		}
		if strings.Contains(got, "evil.example") {
			t.Errorf("joinProbePath(%q) returned %q", path, got)
		}
	}
}

func TestJoinProbePathKeepsATraversalInsideTheHost(t *testing.T) {
	// "/../x" is not a security problem here — it cannot change the host, and
	// the far side decides what the path means. What matters is that the result
	// still points at the configured service.
	got, err := joinProbePath(probeBase, "/../x")
	if err != nil {
		t.Fatalf("joinProbePath: %v", err)
	}
	if !strings.HasPrefix(got, probeBase+"/") {
		t.Errorf("joinProbePath = %q, want it to stay on %s", got, probeBase)
	}
}

func TestJoinProbePathReplacesABasePath(t *testing.T) {
	// An absolute reference replaces the base's path rather than being appended
	// to it — ResolveReference's documented behaviour, and worth pinning because
	// a configured base of ".../v1" does not prefix anything.
	//
	// Not a refusal: the host is what this function guards, and the host is
	// unchanged.
	got, err := joinProbePath("http://payment-service:8080/v1", "/health")
	if err != nil {
		t.Fatalf("joinProbePath: %v", err)
	}
	if got != "http://payment-service:8080/health" {
		t.Errorf("joinProbePath = %q, want the base path replaced", got)
	}
}

func TestHTTPProbeReportsWhatItDidToTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/exact":
			fmt.Fprint(w, strings.Repeat("a", probeBodyBytes))
		case "/big":
			fmt.Fprint(w, strings.Repeat("a", probeBodyBytes*4))
		default:
			fmt.Fprint(w, "ok")
		}
	}))
	defer srv.Close()

	s := New(config.OpsMCP{
		ProbeTargets:   map[string]string{"svc": srv.URL},
		ProbeTimeout:   5 * time.Second,
		ProbeBodyBytes: probeBodyBytes,
	}, log.Discard())

	probe := func(path string) string {
		t.Helper()
		res, _, err := s.httpProbe(t.Context(), nil, HTTPProbeArgs{Service: "svc", Path: path})
		if err != nil {
			t.Fatalf("httpProbe: %v", err)
		}
		return res.Content[0].(*mcp.TextContent).Text
	}

	// A body that ends mid-stream with nothing saying so reads to the model
	// like a service returning malformed output.
	if got := probe("/big"); !strings.Contains(got, "body truncated to the first") {
		t.Errorf("a truncated body was not announced: %q", got[:min(len(got), 200)])
	}
	// A body that exactly fills the cap was not cut and must not claim it was.
	if got := probe("/exact"); strings.Contains(got, "truncated") {
		t.Errorf("a body at exactly the cap was reported as truncated")
	}
	if got := probe("/small"); strings.Contains(got, "truncated") {
		t.Errorf("a short body was reported as truncated")
	}
}
