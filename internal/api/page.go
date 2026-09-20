package api

import (
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/store"
)

// list is the response shape for every paginated listing.
//
// Items is always an array — [] rather than null — so a client never has to
// handle two spellings of "nothing matched". NextCursor is omitted on the last
// page, which is how a client knows to stop.
type list[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// parsePageParams reads ?limit= and ?cursor=, recording any problem in v.
//
// It takes the caller's validation rather than returning its own error so that
// a listing which also has filters reports everything at once: ?limit=0 with a
// malformed ?service= is two problems with the request, and answering with one
// of them would make the client fix it and get the other.
//
// Bad input is rejected rather than corrected. Silently clamping limit=1000 to
// 100 would let a client believe it had received a complete result, which is a
// worse outcome than an error it can see. An absent parameter is not an error:
// it takes the default.
func parsePageParams(c *gin.Context, v *validation) store.PageParams {
	p := store.PageParams{Limit: store.DefaultPageLimit}

	if raw, ok := c.GetQuery("limit"); ok {
		n, err := strconv.Atoi(raw)
		switch {
		case err != nil:
			v.add("limit", httpx.CodeInvalidType, "limit must be an integer")
		case n < 1 || n > store.MaxPageLimit:
			v.add("limit", httpx.CodeInvalidValue,
				"limit must be between 1 and "+itoa(store.MaxPageLimit))
		default:
			p.Limit = n
		}
	}

	if raw, ok := c.GetQuery("cursor"); ok && raw != "" {
		if !id.Valid(raw) {
			v.add("cursor", httpx.CodeInvalidFormat, "cursor must be an id from a previous page")
		} else {
			p.Cursor = raw
		}
	}

	return p
}

// itoa is strconv.Itoa under a shorter name, used to build validation messages.
func itoa(n int) string { return strconv.Itoa(n) }
