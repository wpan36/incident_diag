package api

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/search"
)

// maxQueryLen bounds ?q=.
//
// It is generous relative to any question a person types and small relative to
// what the embedding provider would accept, because the query is embedded and a
// caller pasting a whole document into a query string is not a search this
// endpoint has to serve.
const maxQueryLen = 1024

// search retrieves the chunks nearest to a query.
//
// It exists to make retrieval inspectable — the agent in Phase E calls
// search.Client directly rather than going over HTTP — so it reports what was
// matched and how well, not just the text.
func (s *Server) search(c *gin.Context) {
	var v validation

	// requiredText rather than a check of its own: it trims before deciding
	// the query is absent, and it counts characters. A bespoke len() here
	// would have told a client that a 400-character Chinese question was
	// longer than 1024 characters.
	q := v.requiredText("q", c.Query("q"), maxQueryLen)

	var query search.Query
	if raw, ok := c.GetQuery("service"); ok {
		if service := v.optionalService("service", &raw); service != nil {
			query.Service = *service
		}
	}
	if raw, ok := c.GetQuery("document_type"); ok && raw != "" {
		if !documentTypes[raw] {
			v.add("document_type", httpx.CodeInvalidValue,
				"document_type must be one of runbook, postmortem, service_doc")
		} else {
			query.DocumentType = raw
		}
	}

	query.K = search.DefaultK
	if raw, ok := c.GetQuery("k"); ok {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			v.add("k", httpx.CodeInvalidType, "k must be an integer")
		case n < 1 || n > search.MaxK:
			// Rejected rather than clamped, for the reason parsePageParams
			// gives: a silently reduced result looks complete.
			v.add("k", httpx.CodeInvalidValue, "k must be between 1 and "+itoa(search.MaxK))
		default:
			query.K = n
		}
	}

	if err := v.err(); err != nil {
		renderError(c, err)
		return
	}

	ctx := c.Request.Context()

	// The two dependencies are called separately and their failures are
	// classified separately, which is the whole reason search.Search takes a
	// vector instead of text: "the embedding provider is rate limiting" and
	// "Elasticsearch is down" are different operational problems and a single
	// 503 that does not say which one wastes the first minute of every
	// investigation into it.
	vectors, err := s.deps.Embedder.Embed(ctx, []string{q})
	if err != nil {
		renderError(c, httpx.UnavailableErr(err, "the embedding service is unavailable"))
		return
	}
	if len(vectors) != 1 {
		renderError(c, httpx.Internal(nil, "the embedding service returned no vector"))
		return
	}

	results, err := s.deps.Search.Search(ctx, vectors[0], query)
	if err != nil {
		renderError(c, httpx.UnavailableErr(err, "search is unavailable"))
		return
	}

	c.JSON(http.StatusOK, newSearchResults(results))
}
