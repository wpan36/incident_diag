package mq

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wpan36/incident_diag/internal/id"
)

// SchemaVersion is the version this build produces. A consumer that reads a
// higher one cannot know what it means and treats the message as a permanent
// failure rather than guessing.
const SchemaVersion = 1

// Decoding errors, distinguishable because the handler reacts differently to
// each: an unknown version is a permanent failure to record against the row,
// while malformed JSON names no row at all.
var (
	ErrUnknownSchemaVersion = errors.New("mq: unknown schema version")
	ErrInvalidMessage       = errors.New("mq: invalid message")
)

// Envelope is the part every message shares.
//
// It is embedded, not nested, so the JSON on the wire is flat:
//
//	{"schema_version":1,"message_id":"01JB…","produced_at":"…","document_id":"01JB…"}
//
// A nested payload would buy one Handler signature for every topic — uniformity
// nothing here needs, since there are two topics with one consumer each — and
// would cost a second decode inside every handler.
type Envelope struct {
	SchemaVersion int       `json:"schema_version"`
	MessageID     string    `json:"message_id"`
	ProducedAt    time.Time `json:"produced_at"`
}

// version is the accessor that lets Decode check the version generically. It is
// unexported because nothing outside this package has a reason to ask.
func (e Envelope) version() int { return e.SchemaVersion }

// Message is what Decode can produce. Both methods come from the embedded
// Envelope and from the type's own EntityID, so adding a topic means adding a
// type here rather than loosening anything.
type Message interface {
	version() int

	// EntityID is the row this message is about. It is also the partition key,
	// which is what keeps a redelivery and a reconciler's re-enqueue on one
	// partition and therefore out of two consumers' hands at once.
	EntityID() string
}

// DocumentMessage asks the ingestion worker to ingest one document.
//
// It is thin on purpose. Carrying the service or the document type as well
// would be a second copy of state MySQL owns, and it would be stale the moment
// anything edited the row between produce and consume. The worker reads the row.
type DocumentMessage struct {
	Envelope
	DocumentID string `json:"document_id"`
}

// EntityID implements Message.
func (m DocumentMessage) EntityID() string { return m.DocumentID }

// RunMessage asks the agent worker to execute one run.
type RunMessage struct {
	Envelope
	RunID string `json:"run_id"`
}

// EntityID implements Message.
func (m RunMessage) EntityID() string { return m.RunID }

// NewDocumentMessage builds a message for documentID, stamping the envelope.
//
// Producers never fill an envelope by hand: a message_id that is sometimes
// absent is worse than no message_id at all, because the logs stop being
// joinable exactly when something has gone wrong.
func NewDocumentMessage(documentID string) DocumentMessage {
	return DocumentMessage{Envelope: newEnvelope(), DocumentID: documentID}
}

// NewRunMessage builds a message for runID.
func NewRunMessage(runID string) RunMessage {
	return RunMessage{Envelope: newEnvelope(), RunID: runID}
}

// newEnvelope stamps the fields every message carries.
//
// ProducedAt is truncated to microseconds for the same reason the store
// truncates: it is the precision MySQL's DATETIME(6) keeps, and a timestamp
// that changes when it is written down is a timestamp two logs disagree about.
func newEnvelope() Envelope {
	return Envelope{
		SchemaVersion: SchemaVersion,
		MessageID:     id.New(),
		ProducedAt:    time.Now().UTC().Truncate(time.Microsecond),
	}
}

// Encode renders a message as the JSON that goes on the wire.
func Encode(msg any) ([]byte, error) {
	b, err := json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("mq: encode message: %w", err)
	}
	return b, nil
}

// Decode parses data into T and checks that it is usable.
//
// Unknown fields are ignored, so a later version can add one without every
// older consumer treating it as poison.
//
// On ErrUnknownSchemaVersion the decoded value is returned anyway: the JSON
// parsed, so the entity ID is trustworthy, and the caller needs it to record
// the permanent failure against the right row. Every other error returns the
// zero value, because nothing in it can be relied on.
func Decode[T Message](data []byte) (T, error) {
	var msg T
	if err := json.Unmarshal(data, &msg); err != nil {
		var zero T
		return zero, fmt.Errorf("%w: %v", ErrInvalidMessage, err)
	}
	if !id.Valid(msg.EntityID()) {
		var zero T
		return zero, fmt.Errorf("%w: entity id %q is not a ULID", ErrInvalidMessage, msg.EntityID())
	}
	if v := msg.version(); v != SchemaVersion {
		return msg, fmt.Errorf("%w: got %d, this build speaks %d", ErrUnknownSchemaVersion, v, SchemaVersion)
	}
	return msg, nil
}
