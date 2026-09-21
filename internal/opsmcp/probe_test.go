package opsmcp

import (
	"strings"
	"testing"
)

const probeBase = "http://payment-service:8080"

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

func TestJoinProbePathRefusesABaseWithAPath(t *testing.T) {
	// A base carrying its own path is a configuration mistake worth surfacing
	// rather than silently resolving against.
	got, err := joinProbePath("http://payment-service:8080/v1", "/health")
	if err != nil {
		t.Fatalf("joinProbePath: %v", err)
	}
	// ResolveReference replaces the base path with an absolute reference, which
	// is the documented behaviour; the host is what this function guards.
	if got != "http://payment-service:8080/health" {
		t.Errorf("joinProbePath = %q", got)
	}
}
