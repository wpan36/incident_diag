package opsmcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wpan36/incident_diag/internal/summary"
)

// HTTPProbeArgs is the tool's input.
type HTTPProbeArgs struct {
	Service string `json:"service" jsonschema:"one of the services this server is configured to probe"`
	Path    string `json:"path,omitempty" jsonschema:"absolute path beginning with a single slash, for example /health"`
}

func (s *Server) httpProbe(ctx context.Context, _ *mcp.CallToolRequest, in HTTPProbeArgs) (*mcp.CallToolResult, Meta, error) {
	base, known := s.cfg.ProbeTargets[in.Service]
	if !known {
		res, meta := unknownName("service", in.Service, keysOf(s.cfg.ProbeTargets))
		return res, meta, nil
	}

	path := in.Path
	if path == "" {
		path = "/"
	}
	target, err := joinProbePath(base, path)
	if err != nil {
		res, meta := refused("%v", err)
		return res, meta, nil
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.ProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		res, meta := failed(KindError, "could not build the probe request: %v", err)
		return res, meta, nil
	}

	began := time.Now()
	res, err := s.http.Do(req)
	latency := time.Since(began)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			r, meta := failed(KindTimeout, "%s did not answer within %s", in.Service, s.cfg.ProbeTimeout)
			return r, meta, nil
		}
		r, meta := failed(KindError, "%s did not answer: %v", in.Service, err)
		return r, meta, nil
	}
	defer res.Body.Close()

	// One byte past the cap, so a body that exactly fills it can be told apart
	// from one that was cut. A body that ends mid-sentence with nothing saying
	// so reads to the model like a service returning malformed output.
	raw, _ := io.ReadAll(io.LimitReader(res.Body, int64(s.cfg.ProbeBodyBytes)+1))
	body, _, bodyTruncated := summary.CapAt(string(raw), s.cfg.ProbeBodyBytes)

	// A 5xx is a successful probe: the tool did its job and the answer is that
	// the service is failing, which is exactly what the agent wanted to learn.
	// Only a failure to get any response at all is an error.
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s\nstatus %d\nlatency %dms\ncontent-type %s\n",
		target, res.StatusCode, latency.Milliseconds(),
		firstNonEmpty(res.Header.Get("Content-Type"), "(none)"))
	if location := res.Header.Get("Location"); location != "" {
		// Reported rather than followed, so the agent sees the redirect instead
		// of a body from wherever it pointed.
		fmt.Fprintf(&b, "location %s (not followed)\n", location)
	}
	if bodyTruncated {
		fmt.Fprintf(&b, "body truncated to the first %d bytes\n", s.cfg.ProbeBodyBytes)
	}
	if len(body) > 0 {
		fmt.Fprintf(&b, "\n%s", body)
	}

	out, meta := ok(b.String(), Meta{})
	return out, meta, nil
}

// joinProbePath builds the request URL and refuses anything that could leave
// the configured host.
//
// The "no URL" guarantee does not survive a naive join. With a base of
// http://payment-service:8080/, Go's ResolveReference sends "//evil.example/x"
// and "http://evil.example/x" straight to another host, because both are valid
// URL references. The three rules below reject those spellings, and the host
// equality check afterwards catches whatever they missed.
func joinProbePath(base, path string) (string, error) {
	switch {
	case !strings.HasPrefix(path, "/"):
		return "", fmt.Errorf("path must begin with a single slash, got %q", path)
	case strings.HasPrefix(path, "//"):
		return "", fmt.Errorf("path must not begin with two slashes, got %q", path)
	case strings.Contains(path, "://"):
		return "", fmt.Errorf("path must be a path, not a URL, got %q", path)
	}

	baseURL, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("this server's target for that service is not a URL: %v", err)
	}
	ref, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("path is not a valid URL path: %v", err)
	}

	joined := baseURL.ResolveReference(ref)
	// The belt to the rules' braces. Anything that changed the host or the
	// scheme is refused even if it got past the string checks above, so a
	// spelling nobody thought of still cannot reach another host.
	if joined.Host != baseURL.Host || joined.Scheme != baseURL.Scheme {
		return "", fmt.Errorf("path would leave %s, which is not allowed", baseURL.Host)
	}
	return joined.String(), nil
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
