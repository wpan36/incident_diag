// Command ops-mcp exposes the read-only investigation tools over MCP.
//
// It is a separate process so that the trust boundary is visible rather than
// implied: everything the agent can do to the operational world is what this
// server exposes, and that is legible from the Compose file alone.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/obs"
	"github.com/wpan36/incident_diag/internal/opsmcp"
	"github.com/wpan36/incident_diag/internal/shutdown"
)

// serviceName identifies this binary in traces and metrics.
const serviceName = "ops-mcp"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	// Every loader runs before any error is returned, so an operator with
	// problems in two of them is told about both at once.
	cfg, cfgErr := config.Load()
	httpCfg, httpErr := config.LoadHTTPServer()
	toolCfg, toolErr := config.LoadOpsMCP()
	traceCfg, traceErr := config.LoadTracing(serviceName)
	if err := errors.Join(cfgErr, httpErr, toolErr, traceErr); err != nil {
		return err
	}

	logger := log.New(os.Stdout, cfg.LogLevel, serviceName)
	logger.Info("starting ops-mcp", "config", cfg.String(), "http", httpCfg.String(),
		"tools", toolCfg.String(), "tracing", traceCfg.String())

	ctx, stop := shutdown.Context(context.Background())
	defer stop()

	var closers shutdown.Group

	// Registered first so it shuts down last, giving the spans of everything
	// above it somewhere to go.
	flushTraces, err := obs.Setup(ctx, obs.Config{Endpoint: traceCfg.Endpoint, Service: traceCfg.Service}, logger)
	if err != nil {
		return err
	}
	closers.Add("tracing", flushTraces)

	// Nothing is dialled at startup. Prometheus and the probe targets are
	// reached per call, so a dependency that is down is a tool result the agent
	// can read rather than a server that refuses to start — which is the same
	// position cmd/api takes about Kafka.
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           obs.Handler(opsmcp.New(toolCfg, logger).Handler(), "mcp"),
		ReadHeaderTimeout: httpCfg.ReadHeaderTimeout,
		ReadTimeout:       httpCfg.ReadTimeout,
		// No write timeout. MCP over streamable HTTP holds a response open for
		// the life of a session, and a write deadline would cut it mid-session
		// exactly as it would cut SSE.
		IdleTimeout: httpCfg.IdleTimeout,
	}
	closers.Add("http server", srv.Shutdown)

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		logger.Info("signal received, shutting down")
	}

	if err := closers.Close(httpCfg.ShutdownTimeout); err != nil {
		return fmt.Errorf("shutdown incomplete: %w", err)
	}
	logger.Info("shutdown complete")
	return nil
}
