// Package httpx classifies errors so that an HTTP layer can map them to status
// codes without knowing where they came from.
//
// The problem it solves: a store returns "no rows", a parser returns "invalid
// syntax", a Kafka producer returns a network error, and by the time these
// reach a handler they have been wrapped several times. Without a
// classification, the handler either inspects the layers below it — coupling it
// to their internals — or returns 500 for everything.
//
// What this package deliberately does not define is the JSON response body.
// That is part of the API contract and is decided by the data model and API
// surface specification, not here.
package httpx

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// Kind classifies an error well enough to choose a status code. The set is
// small on purpose: a classification with thirty members stops being applied
// consistently.
type Kind int

const (
	// KindInternal is the zero value, so an unclassified error is treated as a
	// server fault. Failing towards 500 is right: the alternative is leaking a
	// bug to the client as a 400 and never noticing it.
	KindInternal Kind = iota

	// KindInvalid is a malformed or semantically wrong request. 400.
	KindInvalid

	// KindNotFound is a resource that does not exist. 404.
	KindNotFound

	// KindConflict is a request that contradicts current state, such as
	// starting a run on an incident that already has one in flight. 409.
	KindConflict

	// KindUnavailable is a dependency that is down or refusing work. 503.
	KindUnavailable
)

// String returns a stable lowercase name for the kind. It is used in logs and
// metric labels, so the values must not change casually.
func (k Kind) String() string {
	switch k {
	case KindInvalid:
		return "invalid"
	case KindNotFound:
		return "not_found"
	case KindConflict:
		return "conflict"
	case KindUnavailable:
		return "unavailable"
	case KindInternal:
		return "internal"
	default:
		return fmt.Sprintf("kind(%d)", int(k))
	}
}

// Status returns the HTTP status code for the kind.
func (k Kind) Status() int {
	switch k {
	case KindInvalid:
		return http.StatusBadRequest
	case KindNotFound:
		return http.StatusNotFound
	case KindConflict:
		return http.StatusConflict
	case KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Error is a classified error.
//
// Message is written for the caller of the API and must not contain anything
// internal. The wrapped Err is for logs: it keeps the original error reachable
// through errors.Is and errors.As so that lower layers can still be inspected.
type Error struct {
	Kind    Kind
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return e.Message
	}
	return e.Message + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// newf builds an Error whose message is formatted, treating a trailing error
// argument as the cause. Using %w in the format keeps the cause reachable.
func newf(kind Kind, err error, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...), Err: err}
}

// Invalid reports a malformed or semantically wrong request.
func Invalid(format string, args ...any) *Error {
	return newf(KindInvalid, nil, format, args...)
}

// InvalidErr reports a malformed request caused by err.
func InvalidErr(err error, format string, args ...any) *Error {
	return newf(KindInvalid, err, format, args...)
}

// NotFound reports a resource that does not exist.
func NotFound(format string, args ...any) *Error {
	return newf(KindNotFound, nil, format, args...)
}

// NotFoundErr reports a missing resource caused by err, typically sql.ErrNoRows.
func NotFoundErr(err error, format string, args ...any) *Error {
	return newf(KindNotFound, err, format, args...)
}

// Conflict reports a request that contradicts current state.
func Conflict(format string, args ...any) *Error {
	return newf(KindConflict, nil, format, args...)
}

// ConflictErr reports a state conflict caused by err.
func ConflictErr(err error, format string, args ...any) *Error {
	return newf(KindConflict, err, format, args...)
}

// Unavailable reports a dependency that is down or refusing work.
func Unavailable(format string, args ...any) *Error {
	return newf(KindUnavailable, nil, format, args...)
}

// UnavailableErr reports a failing dependency caused by err.
func UnavailableErr(err error, format string, args ...any) *Error {
	return newf(KindUnavailable, err, format, args...)
}

// Internal reports a server-side fault caused by err.
//
// The message is not derived from err on purpose: internal error text routinely
// contains connection strings, query fragments and file paths, none of which
// belong in a response.
func Internal(err error, format string, args ...any) *Error {
	return newf(KindInternal, err, format, args...)
}

// KindOf classifies err, looking through wrapping.
//
// Errors from the standard library that have an obvious classification are
// recognised even when they were not produced by this package, because the
// alternative is every caller remembering to translate them:
//
//   - context.DeadlineExceeded means a dependency did not answer in time,
//     which is an availability problem rather than a bug.
//   - context.Canceled normally means the client went away.
//
// Anything else is KindInternal.
func KindOf(err error) Kind {
	if err == nil {
		return KindInternal
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return KindUnavailable
	}
	return KindInternal
}

// StatusFor returns the HTTP status code for err.
//
// A nil error is a caller mistake rather than a success: handlers call this on
// the error path only. It maps to 500 rather than panicking, because a
// shutdown-time bug in error handling should not take the process down.
func StatusFor(err error) int { return KindOf(err).Status() }

// Message returns the client-safe message for err, or a generic one if err was
// not classified by this package.
//
// The generic fallback is the important part: an unclassified error reaching a
// handler is a bug, and its text must not be echoed to the caller.
func Message(err error) string {
	var e *Error
	if errors.As(err, &e) && e.Message != "" {
		return e.Message
	}
	return "internal server error"
}
