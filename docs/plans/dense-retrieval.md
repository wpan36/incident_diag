# Dense Retrieval

**Tier B spec (M11).** Short by design: S3 already fixed the mapping, the alias and the
chunk shape, and S1 fixed the API conventions this endpoint follows.

## Problem

M9 built the index and M10 fills it, but nothing reads it. Retrieval is what the agent will
depend on in Phase E, and M12 cannot measure anything without it.

## Technical Plan

### `search.Client.Search`

```go
type Query struct {
    Service      string // optional filter, "" means any
    DocumentType string // optional filter, "" means any
    K            int    // how many chunks to return
}

type Result struct {
    Chunk          // Embedding is cleared
    Score float64
}

func (c *Client) Search(ctx context.Context, vector []float32, q Query) ([]Result, error)
```

**It takes a vector, not text.** `internal/search` depends on `internal/embed` for one
constant, `embed.Dimensions`, and nothing else. Giving it an `Embedder` would put the
retrieval path's timeout and retry policy in the search client while the failure it is most
likely to hit — the hosted embedding provider being slow or rate-limiting — belongs to a
different dependency. Keeping them apart means each caller sees the two failures separately
and can say which one happened. Composition is eight lines at each of the two call sites,
which is cheaper than a package that exists to hold them.

The vector length is checked against `embed.Dimensions` before the request, because a
mismatch produces an Elasticsearch error that does not say what is wrong.

Filters are `term` clauses on the `service` and `document_type` keyword fields, passed in
the kNN query's own `filter` so they are applied during the vector search rather than after
it.

`num_candidates` is `max(50, 10*K)`, capped at 1000. Above the corpus size it is free, and
below it costs recall; at this scale being generous is the choice that cannot quietly go
wrong.

`Embedding` is cleared on every result. A thousand floats per hit would dominate both the
JSON response and anything printed while debugging, and no caller needs it.

### `GET /api/search`

| Parameter | | |
| --- | --- | --- |
| `q` | required | non-blank, at most 1024 characters |
| `service` | optional | same validation as the document filter |
| `document_type` | optional | `runbook` \| `postmortem` \| `service_doc` |
| `k` | optional | 1–50, default 10 |

Not cursor-paginated. A kNN result has no stable cursor — re-running the same query after
an index write can reorder it — and re-running is cheap. The response reuses `list[T]`, so
it renders as `{"items": [...]}` with no `next_cursor`, which keeps the response family
consistent without claiming a pagination this endpoint does not have.

The server gains an `embed.Embedder` and a `*search.Client`, so `cmd/api` now needs the
embedding configuration. An inspection endpoint that cannot embed its query would be
useless, so this coupling is the point rather than a cost.

Both dependencies' failures render as 503 through `httpx.Unavailable`. `internal/search`
keeps returning unclassified errors, because the ingestion worker also calls it and
classifies by its own transient-versus-permanent rules; the HTTP boundary is where an
`httpx` kind belongs.

## Verification

- `make check` — parameter validation, including several bad parameters reported together.
- `make test-integration` — index known chunks, search, assert order and that each filter
  narrows the result.
- Manual: upload a runbook, then `curl '.../api/search?q=connection+pool+exhausted'`.
