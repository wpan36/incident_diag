// Package lab is the Incident Lab: two small services that break on demand, so
// that the agent is evaluated against a system which actually fails rather than
// against fixtures.
//
// It is deliberately independent of the rest of the application. These services
// pretend to belong to someone else's estate — their routes, metric names and
// environment variables come from the knowledge corpus in testdata/knowledge,
// not from this repository's conventions — and nothing here imports the store,
// the queue or the API.
package lab

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"

	"github.com/wpan36/incident_diag/internal/shutdown"
)

// Server timeouts. They are constants rather than configuration: the lab has
// one deployment, and the only value that needs thought is the write timeout,
// which has to outlast an injected latency fault.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 15 * time.Second
)

// RouteKind decides what happens around a handler.
type RouteKind int

const (
	// RouteBusiness is measured, logged, and subject to injected faults. It is
	// what the corpus means by an endpoint.
	RouteBusiness RouteKind = iota

	// RouteSystem is measured and logged but never faulted. /health is here on
	// purpose: the CPU runbook's central signal is that saturation slows every
	// endpoint including the ones that call nothing, and that is only visible
	// if /health is in the histogram.
	RouteSystem

	// RouteInternal is neither measured nor logged: /metrics, which would
	// otherwise count the scraper's own traffic as requests, and /fault, which
	// must keep working when the error fault is set to fail everything.
	RouteInternal
)

// Route is one endpoint.
type Route struct {
	// Method is an HTTP method, or empty to match every method.
	Method string

	// Path is the http.ServeMux pattern, wildcards included: "/orders/{id}".
	Path string

	// Label is what appears in the route metric label and the log line. It is
	// separate from Path because a metric label must not carry an order id.
	Label string

	Kind    RouteKind
	Handler http.HandlerFunc
}

// Options configure a lab service.
type Options struct {
	// Service names the process. It is the Prometheus job name, the log file's
	// name and the "service" attribute on every record.
	Service string

	Logger *slog.Logger

	// LatencyFaultInMiddleware applies the latency fault before the handler
	// runs. checkout-service wants that. payment-service does not: it adds the
	// delay inside the simulated processor call, while a pool connection is
	// held, which is the difference that makes the pool fill.
	LatencyFaultInMiddleware bool
}

// App is one lab service: its registry, its fault controller and its routes.
type App struct {
	service          string
	logger           *slog.Logger
	registry         *prometheus.Registry
	requests         *requestMetrics
	faults           *Controller
	latencyInHandler bool
	routes           []Route
}

// New returns a service with /health, /metrics and /fault already registered.
func New(opts Options) *App {
	reg := newRegistry()
	a := &App{
		service:          opts.Service,
		logger:           opts.Logger,
		registry:         reg,
		requests:         newRequestMetrics(reg),
		faults:           NewController(opts.Logger),
		latencyInHandler: opts.LatencyFaultInMiddleware,
	}

	a.Handle(Route{Method: http.MethodGet, Path: "/health", Kind: RouteSystem, Handler: a.health})
	a.Handle(Route{Path: "/metrics", Kind: RouteInternal, Handler: promhttp.HandlerFor(
		reg, promhttp.HandlerOpts{Registry: reg}).ServeHTTP})
	a.Handle(Route{Path: "/fault", Kind: RouteInternal, Handler: a.faults.Handler()})
	return a
}

// Registry is where a service registers its own metrics.
func (a *App) Registry() prometheus.Registerer { return a.registry }

// Gather reads the current values of every registered metric. Tests use it;
// so does anything that wants the numbers without scraping.
func (a *App) Gather() ([]*dto.MetricFamily, error) { return a.registry.Gather() }

// Faults is the process's fault controller.
func (a *App) Faults() *Controller { return a.faults }

// Logger is the service's logger.
func (a *App) Logger() *slog.Logger { return a.logger }

// Handle registers a route. Registering two routes with the same pattern
// panics, which is what http.ServeMux does and is a programmer error either
// way.
func (a *App) Handle(r Route) {
	if r.Label == "" {
		r.Label = r.Path
	}
	a.routes = append(a.routes, r)
}

// Handler builds the mux. It is called once, by Run, and separately by tests.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, r := range a.routes {
		pattern := r.Path
		if r.Method != "" {
			pattern = r.Method + " " + r.Path
		}
		mux.HandleFunc(pattern, a.wrap(r))
	}
	return mux
}

// wrap adds the fault injection, the duration metric and the request log around
// one handler.
func (a *App) wrap(rt Route) http.HandlerFunc {
	if rt.Kind == RouteInternal {
		return rt.Handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}

		switch status, failed := a.injectedFailure(rt); {
		case failed:
			// "before doing any work" is the whole of the error fault: the
			// handler never runs, so nothing downstream sees the request.
			writeError(rec, status, "injected fault")
		default:
			if a.latencyInHandler && rt.Kind == RouteBusiness {
				if d := a.faults.Delay(); d > 0 {
					sleep(r.Context(), d)
				}
			}
			rt.Handler(rec, r)
		}

		elapsed := time.Since(start)
		a.requests.observe(r.Method, rt.Label, strconv.Itoa(rec.status), elapsed)

		level := slog.LevelInfo
		if rec.status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		a.logger.LogAttrs(r.Context(), level, "request",
			slog.String("method", r.Method),
			slog.String("route", rt.Label),
			slog.Int("status", rec.status),
			slog.Int64("duration_ms", elapsed.Milliseconds()),
		)
	}
}

func (a *App) injectedFailure(rt Route) (int, bool) {
	if rt.Kind != RouteBusiness {
		return 0, false
	}
	return a.faults.ErrorStatus()
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": a.service})
}

// Run serves until ctx is cancelled, then drains in-flight requests and stops
// the fault controller's goroutines.
func (a *App) Run(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           a.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
	}

	var closers shutdown.Group
	// Registered first, so it is closed last: the spinning goroutines outlive
	// the server they were slowing down.
	closers.Add("faults", a.faults.Close)
	closers.Add("http server", srv.Shutdown)

	serveErr := make(chan error, 1)
	go func() {
		a.logger.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			closers.Close(shutdownTimeout)
			return fmt.Errorf("serving http: %w", err)
		}
	case <-ctx.Done():
		a.logger.Info("shutting down")
	}

	if err := closers.Close(shutdownTimeout); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	a.logger.Info("stopped")
	return nil
}

// recorder remembers the status code so the middleware can label the metric
// and the log line. A handler that never calls WriteHeader answered 200.
type recorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *recorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.wroteHeader = true
	return r.ResponseWriter.Write(b)
}

// sleep waits for d or until ctx is cancelled, whichever comes first.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}
