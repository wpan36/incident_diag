package api

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/log"
)

// errorBody is the response body for every failure this API produces.
//
//	{"error": {"code": "...", "message": "...", "request_id": "...", "fields": {...}}}
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	// Code is exactly httpx.Kind.String(): invalid, not_found, conflict,
	// unavailable, internal.
	Code string `json:"code"`
	// Message is httpx.Message, which returns a generic string for anything
	// this application did not classify, so internal error text cannot reach a
	// client.
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	// Fields is present only on a validation error.
	Fields map[string]string `json:"fields,omitempty"`
}

// errTooLarge marks a request the client made too big: an uploaded file over
// DOCUMENT_MAX_UPLOAD_BYTES, a multipart envelope over its backstop, or a JSON
// body over maxJSONBodyBytes.
//
// It exists because 413 is the one status this API returns that is not
// httpx.Kind.Status()'s own — the kind is KindInvalid and would derive 400.
// Inventing a sixth kind in httpx for three conditions on two endpoints is not
// worth it, and neither is letting each handler pass its own status: a handler
// that forgets answers 400 to a request it never read, which is a lie a client
// cannot detect. Marking the error instead keeps the exception in renderError,
// stated once.
var errTooLarge = errors.New("request is too large")

// tooLarge builds the client-facing error for something over a limit.
//
// The message is the handler's, because only the handler knows which limit was
// exceeded and what it is; the 413 comes from errTooLarge being reachable
// through errors.Is.
func tooLarge(format string, args ...any) error {
	return httpx.InvalidErr(errTooLarge, format, args...)
}

// renderError writes the error envelope, deriving the status from the error.
func renderError(c *gin.Context, err error) {
	status := httpx.StatusFor(err)
	if errors.Is(err, errTooLarge) {
		status = http.StatusRequestEntityTooLarge
	}
	// Attach the error for the logging middleware. The client sees
	// httpx.Message; the cause, with everything internal still in it, goes to
	// the log record for this request.
	_ = c.Error(err)

	c.AbortWithStatusJSON(status, errorBody{Error: errorDetail{
		Code:      httpx.KindOf(err).String(),
		Message:   httpx.Message(err),
		RequestID: log.RequestID(c.Request.Context()),
		Fields:    httpx.FieldsOf(err),
	}})
}

// renderCreated writes a 201 with a Location header pointing at the new
// resource.
//
// The body is the created resource, not just its id, so a client never has to
// follow up with a GET to learn what it made.
func renderCreated(c *gin.Context, location string, body any) {
	c.Header("Location", location)
	c.JSON(http.StatusCreated, body)
}

// logAttrs describes an error for a log record without duplicating what the
// response already said.
func logAttrs(err error) []slog.Attr {
	return []slog.Attr{
		slog.String("error", err.Error()),
		slog.String("error_kind", httpx.KindOf(err).String()),
	}
}
