// Package log builds the structured logger used across this project and carries
// correlation identifiers through context.
//
// The identifiers are the point. An investigation spans an HTTP request, a
// Kafka message, an agent run and several tool calls in different processes,
// and reconstructing it afterwards is only possible if every record carries the
// same identifiers. Attaching them at each call site would mean relying on
// nobody ever forgetting, so instead they travel in the context and a handler
// puts them on every record automatically.
package log

import (
	"context"
	"io"
	"log/slog"
	"time"
)

// Attribute keys. They are constants because queries in a log backend are
// written against these exact strings.
const (
	KeyRequestID = "request_id"
	KeyRunID     = "run_id"

	// KeyService names the process that emitted the record. It is set once,
	// here, rather than at call sites, and it is part of the log format the
	// MCP tool boundary specification fixes.
	KeyService = "service"
)

// ctxKey is unexported so no other package can collide with these context keys.
type ctxKey int

const (
	requestIDKey ctxKey = iota
	runIDKey
)

// New returns a JSON logger writing to w at the given level, tagged with the
// service name and with correlation identifiers pulled from context on every
// record.
//
// The output is JSON in every environment. Pretty console output would be
// nicer to read locally, but having local logs differ from container logs means
// debugging a problem in one format and reading it in another.
//
// service is a parameter rather than something a binary is expected to add with
// With, for the same reason the identifiers above are not added at call sites:
// a required argument cannot be forgotten. An empty service is allowed and
// omits the attribute, which is what the package's own tests want.
func New(w io.Writer, level slog.Level, service string) *slog.Logger {
	var base slog.Handler = slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: utcMillisecondTime,
	})
	if service != "" {
		base = base.WithAttrs([]slog.Attr{slog.String(KeyService, service)})
	}
	return slog.New(&contextHandler{base: base, resolved: base})
}

// utcMillisecondTime forces the record timestamp to UTC at millisecond
// precision.
//
// Both halves matter to read_service_logs, which filters records from several
// services by time window. Containers do not agree on a time zone, so a local
// timestamp makes that filter silently wrong; the truncation is what the log
// format specifies, and slog would otherwise emit nanoseconds.
//
// Only the top-level time attribute is touched. A time carried as a
// caller-supplied attribute is that caller's data.
func utcMillisecondTime(groups []string, a slog.Attr) slog.Attr {
	if len(groups) != 0 || a.Key != slog.TimeKey {
		return a
	}
	if t, ok := a.Value.Any().(time.Time); ok {
		a.Value = slog.TimeValue(t.UTC().Truncate(time.Millisecond))
	}
	return a
}

// Discard returns a logger that throws everything away. Tests that exercise a
// component's behaviour rather than its logging use this to keep output clean.
func Discard() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// WithRequestID returns a context carrying the HTTP request identifier.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request identifier carried by ctx, or "" if there is
// none.
func RequestID(ctx context.Context) string { return stringValue(ctx, requestIDKey) }

// WithRunID returns a context carrying the agent run identifier.
func WithRunID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, runIDKey, id)
}

// RunID returns the agent run identifier carried by ctx, or "" if there is none.
func RunID(ctx context.Context) string { return stringValue(ctx, runIDKey) }

func stringValue(ctx context.Context, key ctxKey) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(key).(string)
	return s
}

// contextHandler copies correlation identifiers from the context onto each
// record before delegating to the wrapped handler.
//
// The subtlety is WithGroup. A handler that has had a group opened puts every
// record attribute inside that group, so naively adding the identifiers during
// Handle would produce {"detail": {"request_id": ...}} and break any query
// written against a top-level request_id. Correlation identifiers have to sit
// at the top level no matter what grouping a particular logger has applied.
//
// So the handler keeps the original ungrouped handler alongside the chain of
// WithAttrs and WithGroup calls made since. When a group is in play, it applies
// the identifiers to the base handler first and replays the chain on top.
type contextHandler struct {
	base     slog.Handler // as constructed, before any WithAttrs or WithGroup
	ops      []op         // the calls made since, in order
	resolved slog.Handler // base with ops applied; the normal path
	hasGroup bool         // whether ops contains a WithGroup
}

// op is one recorded WithAttrs or WithGroup call.
type op struct {
	group string      // non-empty for WithGroup
	attrs []slog.Attr // non-nil for WithAttrs
}

func (h *contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.resolved.Enabled(ctx, level)
}

func (h *contextHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := contextAttrs(ctx)
	if len(attrs) == 0 {
		return h.resolved.Handle(ctx, r)
	}
	if !h.hasGroup {
		// No group is open, so record attributes land at the top level anyway.
		// This is the common case and stays allocation-free.
		r.AddAttrs(attrs...)
		return h.resolved.Handle(ctx, r)
	}
	// A group is open. Rebuild from the base so the identifiers precede it.
	out := h.base.WithAttrs(attrs)
	for _, o := range h.ops {
		if o.group != "" {
			out = out.WithGroup(o.group)
		} else {
			out = out.WithAttrs(o.attrs)
		}
	}
	return out.Handle(ctx, r)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	return &contextHandler{
		base:     h.base,
		ops:      appendOp(h.ops, op{attrs: attrs}),
		resolved: h.resolved.WithAttrs(attrs),
		hasGroup: h.hasGroup,
	}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return &contextHandler{
		base:     h.base,
		ops:      appendOp(h.ops, op{group: name}),
		resolved: h.resolved.WithGroup(name),
		hasGroup: true,
	}
}

// appendOp copies before appending. Handlers derived from a common parent must
// not share backing storage, or one logger's attributes leak into another's.
func appendOp(ops []op, o op) []op {
	out := make([]op, len(ops), len(ops)+1)
	copy(out, ops)
	return append(out, o)
}

// contextAttrs returns the correlation identifiers carried by ctx.
func contextAttrs(ctx context.Context) []slog.Attr {
	var attrs []slog.Attr
	if id := RequestID(ctx); id != "" {
		attrs = append(attrs, slog.String(KeyRequestID, id))
	}
	if id := RunID(ctx); id != "" {
		attrs = append(attrs, slog.String(KeyRunID, id))
	}
	return attrs
}
