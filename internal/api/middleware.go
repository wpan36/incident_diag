package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/log"
)

// HeaderRequestID is the header carrying the correlation identifier, in and out.
const HeaderRequestID = "X-Request-ID"

// maxRequestIDLen bounds an inbound identifier.
//
// The bound matters because this value is written into every structured log
// record for the request and echoed in the response body, so an unvalidated
// header would be an unbounded client-controlled string in both places.
const maxRequestIDLen = 128

// requestID puts a correlation identifier on the request context, echoes it in
// the response header, and makes it available to the error renderer.
//
// An inbound X-Request-ID is honoured so a trace can span services, but only
// after validation: anything unacceptable is discarded and a fresh ULID
// generated, without failing the request. The header is a convenience, not user
// input worth rejecting a request over.
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader(HeaderRequestID)
		if !validRequestID(rid) {
			rid = id.New()
		}

		c.Request = c.Request.WithContext(log.WithRequestID(c.Request.Context(), rid))
		c.Header(HeaderRequestID, rid)
		c.Next()
	}
}

// validRequestID reports whether an inbound identifier can be trusted verbatim.
//
// The character set excludes everything that could forge a log line or confuse
// a log parser — newlines above all, since each record is one line of JSON.
func validRequestID(s string) bool {
	if s == "" || len(s) > maxRequestIDLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// requestLogger logs one record per request once it has been handled.
//
// The request id is not passed explicitly: internal/log takes it from the
// context, which is also how it reaches records written deeper in the call
// stack.
func requestLogger(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		ctx := c.Request.Context()
		attrs := []slog.Attr{
			slog.String("method", c.Request.Method),
			// FullPath is the route pattern ("/api/incidents/:id"), not the
			// concrete path, so identifiers do not become unbounded label
			// values in whatever reads these records.
			slog.String("route", c.FullPath()),
			slog.String("path", c.Request.URL.Path),
			slog.Int("status", c.Writer.Status()),
			slog.Duration("duration", time.Since(start)),
		}

		level := slog.LevelInfo
		if err := c.Errors.Last(); err != nil {
			attrs = append(attrs, logAttrs(err.Err)...)
			// Only a server-side fault is worth waking anyone for; a 404 or a
			// rejected field is the API working as designed.
			if c.Writer.Status() >= http.StatusInternalServerError {
				level = slog.LevelError
			} else {
				level = slog.LevelWarn
			}
		}
		logger.LogAttrs(ctx, level, "request", attrs...)
	}
}

// recovery turns a panic into the same error envelope as everything else.
//
// gin ships its own recovery middleware, but it writes a bare 500 with no body,
// which would make a panic the one failure a client cannot parse. The stack
// goes to the log, never to the response.
func recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			logger.LogAttrs(c.Request.Context(), slog.LevelError, "panic recovered",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
			)
			renderError(c, httpx.Internal(fmt.Errorf("panic: %v", r), "internal server error"))
		}()
		c.Next()
	}
}
