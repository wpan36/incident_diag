package opsmcp

import (
	"log/slog"
	"net/http"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/obs"
)

// Version is reported to clients during initialization.
const Version = "0.1.0"

// Server holds what the tools need. One instance serves every session: the
// tools are read-only and hold no per-session state, so there is nothing to
// keep apart.
type Server struct {
	cfg    config.OpsMCP
	http   *http.Client
	logger *slog.Logger
}

// New builds the server and registers the tools.
//
// The HTTP client is shared by prometheus_query and http_probe and sets no
// timeout of its own: each tool applies its own deadline through the context,
// and a client-level timeout would cut a request that the tool believed it
// still had time for.
//
// Redirects are refused rather than followed. A redirect is a result worth
// showing the agent, and following one is a way to leave the allowlisted host
// after the host check has already passed.
func New(cfg config.OpsMCP, logger *slog.Logger) *Server {
	return &Server{
		cfg: cfg,
		http: &http.Client{
			Transport: obs.Transport(nil),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger,
	}
}

// MCP returns the configured MCP server.
//
// Callers that serve HTTP should build it once and reuse it; see Handler. One
// server serves any number of sessions, and the tools hold no per-session
// state.
func (s *Server) MCP() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "ops-mcp",
		Title:   "Incident investigation tools",
		Version: Version,
	}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "prometheus_query",
		Description: "Run a PromQL query against this deployment's Prometheus. " +
			"Omit start and end for an instant query; give both for a range query. " +
			"Timestamps are RFC 3339 in UTC; relative times are not accepted.",
	}, s.prometheusQuery)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "http_probe",
		Description: "GET a path on one of this deployment's services and report the status, " +
			"latency and a truncated body. Takes a service name, not a URL. " +
			"Redirects are reported, not followed.",
	}, s.httpProbe)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "read_service_logs",
		Description: "Read a service's structured logs, filtered by time window, minimum " +
			"level and substring. Takes a service name, not a path. Returns the last " +
			"matching records, oldest first. The window is [since, until): a record at " +
			"exactly since is included and one at exactly until is not, so adjacent " +
			"windows tile without repeating a record.",
		InputSchema: logsInputSchema(),
	}, s.readServiceLogs)

	return srv
}

// Handler is the process's whole HTTP surface: MCP at /mcp and health at
// /healthz, matching cmd/api.
//
// Readiness is deliberately absent. This process holds no connection it could
// check: Prometheus and the probe targets are reached per call, and a readiness
// probe that dialled them would report this server unhealthy because something
// else is.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Built once, not per request. The SDK documents that returning the same
	// server is fine, one server serves any number of sessions, and the tools
	// hold no per-session state — so rebuilding it, with its three schema
	// inferences, on every request was pure waste.
	srv := s.MCP()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil))
	mux.Handle("/metrics", obs.MetricsHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	return mux
}

// logsInputSchema is the inferred schema with min_level constrained to the
// levels that exist.
//
// The struct tag can carry a description but not an enum, and a description is
// advice while an enum is a constraint the provider can enforce before the call
// is made. Spending a tool call to be told "min_level must be one of ..." is
// avoidable, so it is avoided.
func logsInputSchema() *jsonschema.Schema {
	schema, err := jsonschema.For[ReadServiceLogsArgs](nil)
	if err != nil {
		// Inference failing means the struct above changed into something the
		// schema package cannot describe, which is a programming error found on
		// the first startup after the change.
		panic("opsmcp: inferring the read_service_logs schema: " + err.Error())
	}
	if prop := schema.Properties["min_level"]; prop != nil {
		prop.Enum = make([]any, 0, len(levels))
		for _, name := range levelNamesOrdered() {
			prop.Enum = append(prop.Enum, name)
		}
	}
	return schema
}
