package httpx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestKindStatus(t *testing.T) {
	cases := []struct {
		kind Kind
		want int
		name string
	}{
		{KindInvalid, http.StatusBadRequest, "invalid"},
		{KindNotFound, http.StatusNotFound, "not_found"},
		{KindConflict, http.StatusConflict, "conflict"},
		{KindUnavailable, http.StatusServiceUnavailable, "unavailable"},
		{KindInternal, http.StatusInternalServerError, "internal"},
	}
	for _, c := range cases {
		if got := c.kind.Status(); got != c.want {
			t.Errorf("%s.Status() = %d, want %d", c.name, got, c.want)
		}
		if got := c.kind.String(); got != c.name {
			t.Errorf("Kind.String() = %q, want %q", got, c.name)
		}
	}
}

func TestZeroKindIsInternal(t *testing.T) {
	// An Error built without setting Kind must fail safe. The opposite default
	// would turn a forgotten field into a 400 that nobody investigates.
	var e Error
	if e.Kind != KindInternal {
		t.Errorf("zero Kind = %v, want KindInternal", e.Kind)
	}
	if got := StatusFor(&e); got != http.StatusInternalServerError {
		t.Errorf("StatusFor(zero Error) = %d, want 500", got)
	}
}

func TestClassificationSurvivesWrapping(t *testing.T) {
	base := NotFoundErr(sql.ErrNoRows, "incident %s not found", "inc-1")

	// Three layers of wrapping, which is realistic: store, service, handler.
	wrapped := fmt.Errorf("loading incident: %w", fmt.Errorf("querying: %w", base))

	if got := StatusFor(wrapped); got != http.StatusNotFound {
		t.Errorf("StatusFor(wrapped) = %d, want 404", got)
	}
	if got := KindOf(wrapped); got != KindNotFound {
		t.Errorf("KindOf(wrapped) = %v, want KindNotFound", got)
	}
	// The original cause stays reachable for logging and for callers that care.
	if !errors.Is(wrapped, sql.ErrNoRows) {
		t.Error("errors.Is(wrapped, sql.ErrNoRows) = false")
	}
	var e *Error
	if !errors.As(wrapped, &e) || e.Message != "incident inc-1 not found" {
		t.Errorf("errors.As gave %+v", e)
	}
}

func TestErrorMessageIncludesTheCause(t *testing.T) {
	// Error() is what goes in logs, so it must carry the cause.
	err := InvalidErr(errors.New("unexpected EOF"), "malformed request body")
	want := "malformed request body: unexpected EOF"
	if got := err.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	// With no cause there should be no dangling separator.
	if got := Invalid("service is required").Error(); got != "service is required" {
		t.Errorf("Error() = %q", got)
	}
}

func TestMessageDoesNotLeakUnclassifiedErrors(t *testing.T) {
	// The case that matters: an unclassified error carrying a connection string
	// reaches a handler. Its text must not reach the client.
	leaky := errors.New(`dial tcp 10.0.0.3:3306: connect: connection refused (user=root password=hunter2)`)

	if got := Message(leaky); got != "internal server error" {
		t.Errorf("Message(unclassified) = %q, want the generic message", got)
	}
	if got := StatusFor(leaky); got != http.StatusInternalServerError {
		t.Errorf("StatusFor(unclassified) = %d, want 500", got)
	}

	// An Internal error keeps the developer-written message, which is safe by
	// construction, while the cause stays available for logs only.
	classified := Internal(leaky, "could not load incident")
	if got := Message(classified); got != "could not load incident" {
		t.Errorf("Message(classified) = %q", got)
	}
	if !errors.Is(classified, leaky) {
		t.Error("Internal() dropped its cause")
	}
}

func TestContextErrorsAreClassifiedAsUnavailable(t *testing.T) {
	// These arrive from every timed-out dependency call in the project, and
	// translating them at each call site is something someone would forget.
	for _, err := range []error{context.DeadlineExceeded, context.Canceled} {
		wrapped := fmt.Errorf("querying prometheus: %w", err)
		if got := KindOf(wrapped); got != KindUnavailable {
			t.Errorf("KindOf(%v) = %v, want KindUnavailable", err, got)
		}
		if got := StatusFor(wrapped); got != http.StatusServiceUnavailable {
			t.Errorf("StatusFor(%v) = %d, want 503", err, got)
		}
	}
}

func TestExplicitClassificationBeatsContextError(t *testing.T) {
	// A deadline wrapped in a deliberate classification must keep the
	// deliberate one: the nearest *Error wins.
	err := ConflictErr(context.DeadlineExceeded, "run already in flight")
	if got := KindOf(err); got != KindConflict {
		t.Errorf("KindOf() = %v, want KindConflict", got)
	}
}

func TestStatusForNilIsInternal(t *testing.T) {
	// Only reachable through a bug in a handler's error path. It must not
	// panic there.
	if got := StatusFor(nil); got != http.StatusInternalServerError {
		t.Errorf("StatusFor(nil) = %d, want 500", got)
	}
	if got := Message(nil); got != "internal server error" {
		t.Errorf("Message(nil) = %q", got)
	}
}

func TestConstructorsSetTheirKind(t *testing.T) {
	cases := []struct {
		err  *Error
		want Kind
	}{
		{Invalid("x"), KindInvalid},
		{InvalidErr(errors.New("c"), "x"), KindInvalid},
		{NotFound("x"), KindNotFound},
		{NotFoundErr(errors.New("c"), "x"), KindNotFound},
		{Conflict("x"), KindConflict},
		{ConflictErr(errors.New("c"), "x"), KindConflict},
		{Unavailable("x"), KindUnavailable},
		{UnavailableErr(errors.New("c"), "x"), KindUnavailable},
		{Internal(errors.New("c"), "x"), KindInternal},
	}
	for _, c := range cases {
		if c.err.Kind != c.want {
			t.Errorf("%q has kind %v, want %v", c.err.Error(), c.err.Kind, c.want)
		}
	}
}

func TestFormattingArguments(t *testing.T) {
	err := NotFound("document %s in service %s", "doc-1", "payment-service")
	if got := err.Message; got != "document doc-1 in service payment-service" {
		t.Errorf("Message = %q", got)
	}
}

func TestInvalidFields(t *testing.T) {
	err := InvalidFields(map[string]string{
		"title":   CodeRequired,
		"service": CodeInvalidFormat,
	}, "request validation failed")

	if got := KindOf(err); got != KindInvalid {
		t.Fatalf("kind = %s, want invalid", got)
	}
	if got := StatusFor(err); got != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", got)
	}

	fields := FieldsOf(fmt.Errorf("wrapped: %w", err))
	if len(fields) != 2 || fields["title"] != CodeRequired || fields["service"] != CodeInvalidFormat {
		t.Fatalf("fields = %v, want both reason codes to survive wrapping", fields)
	}
}

func TestFieldsOfIsNilForEveryOtherError(t *testing.T) {
	for _, err := range []error{
		errors.New("plain"),
		Invalid("title is required"),
		NotFound("incident not found"),
		Internal(errors.New("boom"), "internal server error"),
	} {
		if got := FieldsOf(err); got != nil {
			t.Errorf("FieldsOf(%v) = %v, want nil", err, got)
		}
	}
}
