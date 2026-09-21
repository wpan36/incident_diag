// Package opsmcp is the read-only tool surface the agent reaches the
// operational world through.
//
// No tool accepts a path or a URL. Each takes a service name that the server
// resolves against its own configuration, so path traversal is not defended
// against — it cannot be expressed. See docs/plans/mcp-tool-boundary.md.
package opsmcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/wpan36/incident_diag/internal/summary"
)

// Result kinds, which M20 maps onto tool_calls.status.
const (
	KindOK      = "ok"
	KindError   = "error"
	KindTimeout = "timeout"
)

// Meta is the structured half of every tool result.
//
// It exists so the audit trail does not depend on parsing the prose the model
// reads. M20 maps the first four fields onto the tool_calls columns S1 fixed;
// the counters are for whoever is reading a run afterwards.
type Meta struct {
	Kind          string `json:"kind"`
	OriginalBytes int    `json:"original_bytes"`
	Truncated     bool   `json:"truncated"`
	Note          string `json:"note,omitempty"`

	// Refused marks a result the model can fix by itself: an unknown service,
	// a malformed timestamp, a limit exceeded.
	//
	// Such a result is Kind ok and not isError, because the tool ran and
	// declined — the spec couples isError to Kind and this does not break that
	// coupling. Without this field the audit trail could not distinguish "the
	// agent asked correctly" from "the agent asked wrongly and was told so",
	// which is exactly what the agent evaluation wants to count.
	Refused bool `json:"refused,omitempty"`

	// Per-tool counters, omitted where they do not apply.
	Series           int `json:"series,omitempty"`
	Points           int `json:"points,omitempty"`
	Matched          int `json:"matched,omitempty"`
	Returned         int `json:"returned,omitempty"`
	UnparseableLines int `json:"unparseable_lines,omitempty"`
}

// truncationMarker tells the model the text stops short of the answer rather
// than ending there. It counts against the cap: the 8 KiB is the size of what
// leaves this server, not the size of the part before the note saying so.
const truncationMarker = "\n\n[truncated]"

// ok builds a successful result. The cap is applied here, once, so no tool can
// forget it and no two tools can disagree about where it falls.
func ok(text string, meta Meta) (*mcp.CallToolResult, Meta) {
	capped, original, truncated := summary.Cap(text)
	if truncated {
		// Re-cut with room for the marker. Two passes only when it is needed,
		// so text that fits is never trimmed to make space for a note it will
		// not carry.
		capped, _, _ = summary.CapAt(text, summary.LimitBytes-len(truncationMarker))
		capped += truncationMarker
	}
	meta.Kind = KindOK
	meta.OriginalBytes = original
	meta.Truncated = truncated
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: capped}}}, meta
}

// refused answers an argument the model can correct on its next step.
//
// It is a normal result, not an MCP error: an error response is
// indistinguishable from the server being broken, and the model would have
// spent one of its bounded tool calls learning nothing.
func refused(format string, args ...any) (*mcp.CallToolResult, Meta) {
	note := fmt.Sprintf(format, args...)
	res, meta := ok(note, Meta{Note: note})
	meta.Refused = true
	return res, meta
}

// unknownName is refused with the valid names listed, so the next attempt can
// be right rather than being another guess.
func unknownName(param, got string, valid []string) (*mcp.CallToolResult, Meta) {
	sorted := append([]string(nil), valid...)
	sort.Strings(sorted)
	return refused("unknown %s %q; this server knows: %s", param, got, strings.Join(sorted, ", "))
}

// failed answers a dependency that did not answer. This is the case where
// isError is set: the model cannot fix it, and pretending otherwise would have
// it retry a broken Prometheus until its budget ran out.
func failed(kind string, format string, args ...any) (*mcp.CallToolResult, Meta) {
	note := fmt.Sprintf(format, args...)
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: note}},
	}, Meta{Kind: kind, Note: note, OriginalBytes: len(note)}
}
