package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/store"
)

// createIncidentRequest is the body of POST /api/incidents.
//
// Service is a pointer so that an absent key and an explicit null are both nil,
// and so a client that sends a number instead of a string produces a type error
// naming the field rather than a silent zero value.
type createIncidentRequest struct {
	Title       string  `json:"title"`
	Description string  `json:"description"`
	Service     *string `json:"service"`
}

func (s *Server) createIncident(c *gin.Context) {
	var req createIncidentRequest
	if err := decodeJSON(c, &req); err != nil {
		renderError(c, err)
		return
	}

	var v validation
	title := v.requiredText("title", req.Title, maxTitleLen)
	description := v.optionalText("description", req.Description, maxDescriptionLen)
	service := v.optionalService("service", req.Service)
	if err := v.err(); err != nil {
		renderError(c, err)
		return
	}

	inc, err := s.deps.Store.CreateIncident(c.Request.Context(), store.NewIncident{
		Title:       title,
		Description: description,
		Service:     service,
	})
	if err != nil {
		renderError(c, err)
		return
	}
	renderCreated(c, "/api/incidents/"+inc.ID, newIncident(inc))
}

func (s *Server) listIncidents(c *gin.Context) {
	var v validation
	p := parsePageParams(c, &v)
	if err := v.err(); err != nil {
		renderError(c, err)
		return
	}

	page, err := s.deps.Store.ListIncidents(c.Request.Context(), p)
	if err != nil {
		renderError(c, err)
		return
	}
	c.JSON(http.StatusOK, newIncidentList(page))
}

func (s *Server) getIncident(c *gin.Context) {
	incidentID := c.Param("id")
	// Checking the shape first turns a malformed id into a 404 without a round
	// trip to the database, and keeps a nonsense path parameter from reaching
	// SQL at all. It is 404 rather than 400 because "no such incident" is the
	// honest answer either way.
	if !validID(incidentID) {
		renderError(c, httpx.NotFound("incident %s not found", incidentID))
		return
	}

	inc, err := s.deps.Store.GetIncident(c.Request.Context(), incidentID)
	if err != nil {
		renderError(c, err)
		return
	}
	c.JSON(http.StatusOK, newIncident(inc))
}

// maxJSONBodyBytes bounds a JSON request body.
//
// Without it the decoder reads whatever the client sends into memory before
// any validation gets the chance to reject it: a 200 MB body took this process
// from 40 MB resident to 688 MB and then answered 400, which is a
// whole-process cost inflicted by one request. The multipart endpoint has
// always been bounded; this is the same backstop for the JSON ones.
//
// 64 KiB is far above anything legitimate. The largest valid body is an
// incident, and its longest fields are a 255-character title and an
// 8192-character description — at most 33 KiB of UTF-8 even when every
// character is four bytes. The one shape that would be refused unfairly is a
// maximal description in which the client escaped every character as \uXXXX,
// and no client does that.
const maxJSONBodyBytes = 64 << 10

// decodeJSON reads a JSON request body, turning the ways it can be wrong into
// the same envelope as everything else.
//
// A type mismatch is reported against the field that caused it, because
// "service must be a string" is actionable and "invalid character" is not.
func decodeJSON(c *gin.Context, dst any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxJSONBodyBytes)

	err := json.NewDecoder(c.Request.Body).Decode(dst)
	if err == nil {
		return nil
	}

	// Checked before the type error below, because a body cut off at the
	// limit usually also fails to parse, and "the body is too large" is the
	// useful half of that.
	if isTooLarge(err) {
		return tooLarge("request body must be at most %d bytes", maxJSONBodyBytes)
	}

	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return httpx.InvalidFields(
			map[string]string{typeErr.Field: httpx.CodeInvalidType},
			"%s must be a %s", typeErr.Field, typeErr.Type)
	}
	return httpx.InvalidErr(err, "request body is not valid JSON")
}
