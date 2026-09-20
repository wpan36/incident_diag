package api

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/wpan36/incident_diag/internal/httpx"
)

// Field limits, in characters, matching the columns they end up in.
//
// Characters rather than bytes is not a detail. MySQL counts VARCHAR(255) in
// characters, so counting bytes here would reject a 100-character Chinese
// title that the column would have stored without complaint — and tell it, in
// so many words, that it was longer than 255 characters. Every length check in
// this package therefore goes through utf8.RuneCountInString.
//
// The description column is TEXT, which is 65535 *bytes*, so 8192 characters
// fits there in the worst case too.
const (
	maxTitleLen       = 255
	maxDescriptionLen = 8192
)

// serviceNamePattern is the shape of a service identifier: lowercase, starting
// alphanumeric, hyphen-separated, at most 64 characters.
//
// It is validated rather than accepted freely because this value is a retrieval
// filter and a Prometheus label in later milestones, and a corpus where
// "payment-service" and "Payment Service" are different services filters badly.
var serviceNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// validation collects every problem with a request instead of stopping at the
// first, so a client is not made to discover them one round trip at a time.
//
// The zero value is ready to use.
type validation struct {
	fields map[string]string
	// first is the message for the single-field case, remembered in the order
	// fields were checked so the message does not depend on map iteration.
	first string
}

// add records that field is wrong, with a reason code from the closed set in
// httpx and a human message used only when this turns out to be the sole
// problem.
//
// The first complaint about a field wins. A later, vaguer one must not replace
// it: an upload whose filename has the wrong extension has already been told
// something specific, and the "file is required" check that follows — true,
// because nothing was stored — would otherwise overwrite it with a reason code
// that sends the client looking for the wrong mistake.
func (v *validation) add(field, code, message string) {
	if v.fields == nil {
		v.fields = make(map[string]string)
	}
	if _, seen := v.fields[field]; seen {
		return
	}
	if len(v.fields) == 0 {
		v.first = message
	}
	v.fields[field] = code
}

// err returns the collected failure, or nil if there was none.
//
// When exactly one field is at fault the message describes it — "title is
// required". When more than one is, the message is fixed and the detail lives
// in fields, rather than the message growing a comma-joined list that no client
// can parse and every client ends up displaying.
func (v *validation) err() error {
	switch len(v.fields) {
	case 0:
		return nil
	case 1:
		return httpx.InvalidFields(v.fields, "%s", v.first)
	default:
		return httpx.InvalidFields(v.fields, "request validation failed")
	}
}

// requiredText trims value and checks it is present and short enough.
//
// Trimming before the emptiness check is the point: a title of "   " is
// required, not a three-character title.
func (v *validation) requiredText(field, value string, max int) string {
	value = strings.TrimSpace(value)
	switch {
	case value == "":
		v.add(field, httpx.CodeRequired, field+" is required")
	case utf8.RuneCountInString(value) > max:
		v.add(field, httpx.CodeTooLong, field+" must be at most "+itoa(max)+" characters")
	}
	return value
}

// optionalText checks a value that may be absent but must be short enough when
// present. It is not trimmed: the body of a description is the client's.
func (v *validation) optionalText(field, value string, max int) string {
	if utf8.RuneCountInString(value) > max {
		v.add(field, httpx.CodeTooLong, field+" must be at most "+itoa(max)+" characters")
	}
	return value
}

// optionalService validates a service name, returning nil when none was given.
//
// A value that is only whitespace is treated as absent rather than as a
// malformed name: the column is nullable, and "sent an empty box" is the same
// intent as "sent nothing".
func (v *validation) optionalService(field string, value *string) *string {
	if value == nil {
		return nil
	}
	s := strings.TrimSpace(*value)
	if s == "" {
		return nil
	}
	if !serviceNamePattern.MatchString(s) {
		v.add(field, httpx.CodeInvalidFormat,
			field+" must be lowercase alphanumeric with hyphens, at most 64 characters")
		return nil
	}
	return &s
}
