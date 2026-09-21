package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/summary"
)

// defaultK is how many passages search_knowledge returns when the model does
// not say.
//
// Five rather than search.DefaultK's ten: the observation cap is 8 KiB,
// roughly 2000 tokens, and a chunk runs to about 400. Ten hits do not fit and
// would be dropped again at the bottom of the observation, which spends the
// retrieval on passages the model never sees. It is resolved here rather than
// left to search.clampK, which reads a zero as "unset" and answers with its
// own default.
const defaultK = 5

// Knowledge is search_knowledge: this system's own retrieval, composing embed
// and search exactly as GET /api/search does.
//
// It is in the agent rather than behind MCP because it needs both of those in
// process. Putting it behind MCP would send the agent's own retrieval over
// HTTP for nothing, and ops-mcp is the boundary for the operational world,
// not for this system's own storage.
type Knowledge struct {
	Embedder embed.Embedder
	Search   ChunkSearcher
}

// knowledgeArgs is what the model passes.
type knowledgeArgs struct {
	Query        string `json:"query"`
	Service      string `json:"service"`
	DocumentType string `json:"document_type"`
	K            int    `json:"k"`
}

// retrieve runs one search.
//
// A failed retrieval is not a failed run: the failure text becomes the
// observation, the reason goes in agent_steps.error, and the step is still
// OK. The loop carries on, exactly as it does for a failing MCP tool — ERROR
// on a step is reserved for a response that called no tool at all.
func (k *Knowledge) retrieve(ctx context.Context, raw json.RawMessage) Observation {
	var args knowledgeArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return failedRetrieval("", fmt.Sprintf("the arguments to %s are not valid JSON: %v",
				ToolSearchKnowledge, err))
		}
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return failedRetrieval("", ToolSearchKnowledge+" needs a non-empty query.")
	}
	if args.K <= 0 {
		args.K = defaultK
	}

	vectors, err := k.Embedder.Embed(ctx, []string{args.Query})
	if err != nil {
		return failedRetrieval(args.Query, fmt.Sprintf("the search could not be run: %v", err))
	}
	if len(vectors) != 1 {
		return failedRetrieval(args.Query, "the search could not be run: the embedding service returned no vector")
	}

	hits, err := k.Search.Search(ctx, vectors[0], search.Query{
		Service:      args.Service,
		DocumentType: args.DocumentType,
		K:            args.K,
	})
	if err != nil {
		return failedRetrieval(args.Query, fmt.Sprintf("the search could not be run: %v", err))
	}

	return Observation{Text: renderHits(hits), Hits: hits, Query: args.Query}
}

func failedRetrieval(query, reason string) Observation {
	return Observation{Text: reason, Error: reason, Query: query}
}

// renderHits lays the hits out one block per hit, in rank order.
//
// Hits are dropped whole from the end until the text fits the observation cap,
// and the block says so when any were. Cutting mid-hit would hand the model
// half a chunk and a document id it cannot use.
func renderHits(hits []search.Result) string {
	if len(hits) == 0 {
		return "No documents matched the query."
	}
	for shown := len(hits); shown > 0; shown-- {
		text := renderSomeHits(hits, shown)
		if len(text) <= summary.LimitBytes {
			return text
		}
	}
	// One hit larger than the whole cap is possible only for a chunk far above
	// the chunker's target, but this function has to be total, and an observation
	// the store would silently truncate anyway is better rendered truncated here.
	text, _, _ := summary.Cap(renderSomeHits(hits, 1))
	return text
}

func renderSomeHits(hits []search.Result, shown int) string {
	var b strings.Builder
	for i, h := range hits[:shown] {
		if i > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(hitHeader(h))
		b.WriteString("\n")
		b.WriteString(h.Content)
	}
	if shown < len(hits) {
		fmt.Fprintf(&b, "\n\n(%d of %d hits shown; the rest did not fit)", shown, len(hits))
	}
	return b.String()
}

// hitHeader is the one line that identifies a hit. The bracketed value is the
// document_id, which is what finish cites.
func hitHeader(h search.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s", h.DocumentID, h.Source)
	if h.HeadingPath != "" {
		fmt.Fprintf(&b, " · %s", h.HeadingPath)
	}
	fmt.Fprintf(&b, " (%.2f)", h.Score)
	return b.String()
}
