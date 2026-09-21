// Package mcpclient talks to ops-mcp and turns its results into the shape the
// agent's audit trail needs.
//
// It knows the meta envelope that the tool boundary spec defines, and nothing
// else about the individual tools: the tool names, their schemas and their
// descriptions are discovered at connection time and handed to the model as
// they are. Adding a tool to ops-mcp therefore needs no change here.
package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Status values, matching tool_calls.status.
const (
	StatusOK      = "OK"
	StatusError   = "ERROR"
	StatusTimeout = "TIMEOUT"
)

// Tool is one tool the server exposes.
//
// InputSchema is carried as raw JSON because it goes to the model unchanged.
// Re-modelling a JSON Schema in Go only to marshal it back would be a second
// place for the two to disagree.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Result is one tool call, in the terms the store records.
type Result struct {
	// Text is what the model reads.
	Text string

	// Status, OriginalBytes and Truncated come from the server's meta envelope
	// and map onto tool_calls. They are read from structured content rather
	// than parsed out of Text, so the audit trail does not depend on prose.
	Status        string
	OriginalBytes int
	Truncated     bool

	// Refused marks a result the model can fix by itself — an unknown service,
	// a malformed timestamp, a limit exceeded. The call succeeded; the tool
	// declined.
	Refused bool

	// Note is the server's one-line reason, when it gave one.
	Note string
}

// Client is a connected MCP session.
type Client struct {
	session *mcp.ClientSession
	tools   []Tool
	timeout time.Duration
	logger  *slog.Logger
}

// Connect opens a session and lists the tools once.
//
// Listing once is deliberate: ops-mcp registers its tools at startup and never
// changes them, so re-listing per run would be a request per run to learn
// nothing. A tool added to a running server is not picked up until the agent
// worker restarts, which is the same deployment step that added it.
func Connect(ctx context.Context, endpoint string, timeout time.Duration, logger *slog.Logger) (*Client, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "incident-diag", Version: "0.1.0"}, nil)

	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: connect to %s: %w", endpoint, err)
	}

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		session.Close()
		return nil, fmt.Errorf("mcpclient: list tools: %w", err)
	}

	tools := make([]Tool, 0, len(listed.Tools))
	for _, t := range listed.Tools {
		schema, err := json.Marshal(t.InputSchema)
		if err != nil {
			session.Close()
			return nil, fmt.Errorf("mcpclient: tool %s has a schema that will not marshal: %w", t.Name, err)
		}
		tools = append(tools, Tool{Name: t.Name, Description: t.Description, InputSchema: schema})
	}
	if len(tools) == 0 {
		session.Close()
		return nil, fmt.Errorf("mcpclient: %s exposes no tools", endpoint)
	}

	logger.Info("connected to the tool server", "endpoint", endpoint, "tools", toolNames(tools))
	return &Client{session: session, tools: tools, timeout: timeout, logger: logger}, nil
}

// Tools returns what the server exposes, in the order it listed them.
func (c *Client) Tools() []Tool { return c.tools }

// Call invokes a tool.
//
// It returns an error only when the call itself failed — the transport, or a
// tool name the server does not have. A tool that ran and reported a problem is
// a Result with a non-OK status, because that is something the agent reasons
// about rather than something that should end its run.
func (c *Client) Call(ctx context.Context, name string, args json.RawMessage) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var decoded any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &decoded); err != nil {
			return Result{}, fmt.Errorf("mcpclient: arguments for %s are not JSON: %w", name, err)
		}
	}

	res, err := c.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: decoded})
	if err != nil {
		return Result{}, fmt.Errorf("mcpclient: call %s: %w", name, err)
	}
	return c.result(name, res), nil
}

// Close ends the session.
func (c *Client) Close() error { return c.session.Close() }

// result turns a CallToolResult into the store's terms.
func (c *Client) result(name string, res *mcp.CallToolResult) Result {
	out := Result{Text: textOf(res), Status: StatusOK}

	meta, err := decodeMeta(res.StructuredContent)
	switch {
	case err != nil:
		// A result this client cannot account for still reaches the model, but
		// the audit row says the accounting is missing rather than inventing
		// numbers for it.
		c.logger.Warn("tool result has no usable meta", "tool", name, "error", err)
		out.OriginalBytes = len(out.Text)
	default:
		out.Status = statusFor(meta.Kind)
		out.OriginalBytes = meta.OriginalBytes
		out.Truncated = meta.Truncated
		out.Refused = meta.Refused
		out.Note = meta.Note
	}

	// isError and a non-ok kind should always agree; if they do not, the flag
	// the server set explicitly wins, because a result marked as an error must
	// never be recorded as a success.
	if res.IsError && out.Status == StatusOK {
		out.Status = StatusError
	}
	return out
}

// meta mirrors the envelope ops-mcp returns. Only the fields the store needs
// are here: the per-tool counters are for a human reading the run.
type meta struct {
	Kind          string `json:"kind"`
	OriginalBytes int    `json:"original_bytes"`
	Truncated     bool   `json:"truncated"`
	Refused       bool   `json:"refused"`
	Note          string `json:"note"`
}

func decodeMeta(structured any) (meta, error) {
	if structured == nil {
		return meta{}, fmt.Errorf("the result carries no structured content")
	}
	raw, err := json.Marshal(structured)
	if err != nil {
		return meta{}, err
	}
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return meta{}, err
	}
	if m.Kind == "" {
		return meta{}, fmt.Errorf("the structured content has no kind")
	}
	return m, nil
}

func statusFor(kind string) string {
	switch kind {
	case "timeout":
		return StatusTimeout
	case "error":
		return StatusError
	default:
		return StatusOK
	}
}

// textOf concatenates the text blocks. Non-text content is ignored: these tools
// return prose, and silently rendering an image as a placeholder would be worse
// than leaving it out.
func textOf(res *mcp.CallToolResult) string {
	var parts []string
	for _, content := range res.Content {
		if t, isText := content.(*mcp.TextContent); isText {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func toolNames(tools []Tool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name)
	}
	return out
}
