package opsmcp

import (
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wpan36/incident_diag/internal/config"
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
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger,
	}
}

// MCP returns the configured MCP server.
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
			"matching records, oldest first.",
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
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s.MCP() }, nil))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}` + "\n"))
	})
	return mux
}
