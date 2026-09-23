package obs

import (
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Transport wraps an HTTP transport so that outgoing requests carry the trace
// context and appear as client spans.
//
// A nil base means http.DefaultTransport, which is what a zero http.Client
// already uses. When tracing is disabled the wrapper still runs: it writes
// headers from a no-op span context, which costs a map lookup and propagates
// nothing.
func Transport(base http.RoundTripper) http.RoundTripper {
	return otelhttp.NewTransport(base)
}

// Handler wraps an HTTP handler so that incoming requests continue the trace
// their caller started.
//
// cmd/ops-mcp uses this rather than the otelgin middleware internal/api uses:
// it serves one MCP handler and has no router, so there is no gin to hang a
// middleware on. The name is the span name for every request, which is right
// for a single-endpoint server and would be wrong for a router.
func Handler(h http.Handler, name string) http.Handler {
	return otelhttp.NewHandler(h, name, otelhttp.WithFilter(TraceRequest))
}

// TraceRequest reports whether a request is worth a span.
//
// Scrapes and probes are not: Prometheus polls every fifteen seconds, and
// leaving them in means the trace of the run someone is looking for is buried
// under hundreds of scrapes. internal/api passes this to otelgin for the same
// reason.
func TraceRequest(r *http.Request) bool {
	switch r.URL.Path {
	case "/metrics", "/healthz", "/readyz":
		return false
	}
	return true
}
