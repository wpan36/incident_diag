// Package id generates the identifiers used for every row in this project.
//
// They are ULIDs rendered as 26 Crockford base32 characters. Three properties
// are being bought:
//
//   - They sort by creation time, so the primary key doubles as a pagination
//     cursor and no separate ordering column is needed.
//   - They are readable in a log line or a URL without decoding, which a
//     BINARY(16) UUID is not.
//   - The application knows the value before the INSERT, which matters as soon
//     as a row and a Kafka message carrying its ID have to be produced
//     together.
package id

import (
	"crypto/rand"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// entropy is shared and mutex-guarded because monotonicity is a property of a
// single source: two independent generators can produce out-of-order IDs
// within the same millisecond, which would break cursor pagination.
//
// ulid.MonotonicEntropy increments the random component instead of redrawing it
// when several IDs fall in the same millisecond, so IDs generated in a tight
// loop still sort in the order they were created.
var (
	mu      sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 0)
)

// New returns a new ULID for the current time.
//
// It panics if the entropy source fails, which for crypto/rand means the
// operating system has stopped providing randomness. There is no useful
// recovery from that, and returning an error would put an "impossible" branch
// at every call site.
func New() string { return NewAt(time.Now()) }

// NewAt returns a new ULID whose timestamp component is t. Tests use it to
// produce identifiers in a known order.
func NewAt(t time.Time) string {
	mu.Lock()
	defer mu.Unlock()
	return ulid.MustNew(ulid.Timestamp(t), entropy).String()
}

// Valid reports whether s is a well-formed ULID.
//
// It is used to reject a pagination cursor before it reaches SQL. A malformed
// cursor is a client error, not an empty page.
func Valid(s string) bool {
	if len(s) != ulid.EncodedSize {
		return false
	}
	_, err := ulid.ParseStrict(s)
	return err == nil
}
