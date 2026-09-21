// Package api is the HTTP surface: routing, middleware, request validation and
// the response shapes.
//
// Two things hold across every endpoint. Every failure renders the same
// envelope, built from the error's httpx classification rather than from
// whatever the handler happened to know. And every response timestamp goes
// through wire.Time, so the format is a property of the type instead of
// something each handler has to remember.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/files"
	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/store"
)

// readinessTimeout bounds the database check behind /readyz.
//
// It is short on purpose: an orchestrator asking whether this process can serve
// traffic wants an answer now, and a probe that blocks for as long as a query
// might is a probe that reports "unhealthy" by timing out at the caller.
const readinessTimeout = 2 * time.Second

// Deps is what the handlers need, mirroring ingest.Deps.
//
// A struct rather than six positional parameters: three of them are interfaces
// or pointers that tests pass as nil or as a fake, and positional arguments of
// the same shape are how the wrong one gets passed without the compiler
// noticing.
//
// Producer is here because creating a document is half of a dual write: the row
// and the message that tells a worker about it. The handler does not treat the
// message as part of the transaction — see uploadDocument — but it does have to
// try.
//
// Embedder and Search are both required by GET /api/search and by nothing else.
// That endpoint exists to make retrieval inspectable, so an API process that
// could not embed a query would be missing the point rather than saving a
// dependency.
//
// Agent and LLMModel are what POST /api/incidents/{id}/runs records on the
// run. They are here rather than read per request because a run has to keep
// the bounds it was created with, and because that is what makes the API
// process need AGENT_* and LLM_MODEL at all — it runs no agent itself.
type Deps struct {
	Store    *store.Store
	Files    *files.Storage
	Producer mq.Producer
	Embedder embed.Embedder
	Search   *search.Client
	Agent    config.Agent
	LLMModel string
	Logger   *slog.Logger
}

// Server holds what the handlers need. It is constructed once at startup.
type Server struct {
	deps Deps
}

// NewServer builds the server and its router.
func NewServer(deps Deps) *Server { return &Server{deps: deps} }

// Router returns the configured HTTP handler.
//
// gin.New rather than gin.Default: the default engine installs gin's own
// logger and recovery, which would write a second, differently shaped line per
// request and answer a panic with an unparseable body.
func (s *Server) Router() http.Handler {
	r := gin.New()

	// Order matters. requestID runs first so everything after it — including
	// the log record and the error envelope — can see the identifier; recovery
	// sits inside the logger so a panic still produces a request record.
	r.Use(requestID(), requestLogger(s.deps.Logger), recovery(s.deps.Logger))

	r.NoRoute(func(c *gin.Context) {
		renderError(c, httpx.NotFound("no such endpoint"))
	})
	r.NoMethod(func(c *gin.Context) {
		renderError(c, httpx.NotFound("no such endpoint"))
	})

	r.GET("/healthz", s.healthz)
	r.GET("/readyz", s.readyz)

	api := r.Group("/api")
	{
		api.POST("/incidents", s.createIncident)
		api.GET("/incidents", s.listIncidents)
		api.GET("/incidents/:id", s.getIncident)

		api.POST("/incidents/:id/runs", s.createRun)
		api.GET("/incidents/:id/runs", s.listIncidentRuns)

		api.POST("/documents", s.uploadDocument)
		api.GET("/documents", s.listDocuments)
		api.GET("/documents/:id", s.getDocument)

		api.GET("/runs/:id", s.getRun)

		api.GET("/search", s.search)
	}
	return r
}

// healthz is liveness: it touches nothing.
//
// It is separate from readiness so that a brief MySQL outage does not cause the
// orchestrator to restart an otherwise healthy process.
func (s *Server) healthz(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// readyz is readiness: it pings MySQL.
//
// The body is deliberately not extended with which dependency failed. A
// readiness probe that reports that is a monitoring endpoint wearing the wrong
// hat, and it publishes the shape of the deployment to anyone who can reach the
// port.
func (s *Server) readyz(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), readinessTimeout)
	defer cancel()

	if err := s.deps.Store.Ping(ctx); err != nil {
		renderError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// validID reports whether a path parameter could be an identifier this
// application generated.
func validID(s string) bool { return id.Valid(s) }
