package mq

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wpan36/incident_diag/internal/id"
)

func TestDocumentMessageIsFlatOnTheWire(t *testing.T) {
	msg := NewDocumentMessage(id.New())

	b, err := Encode(msg)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	// The shape matters as much as the content: a nested payload would be a
	// different contract, and the spec's example is what another implementation
	// would be written against.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("encoded message is not a JSON object: %v (%s)", err, b)
	}
	for _, key := range []string{"schema_version", "message_id", "produced_at", "document_id"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("encoded message has no top-level %q: %s", key, b)
		}
	}
	if len(raw) != 4 {
		t.Errorf("encoded message has %d keys, want 4: %s", len(raw), b)
	}
}

func TestNewDocumentMessageStampsTheEnvelope(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)
	msg := NewDocumentMessage(id.New())

	if msg.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", msg.SchemaVersion, SchemaVersion)
	}
	if !id.Valid(msg.MessageID) {
		t.Errorf("MessageID = %q, want a ULID", msg.MessageID)
	}
	if msg.ProducedAt.Before(before) || msg.ProducedAt.After(time.Now().UTC().Add(time.Second)) {
		t.Errorf("ProducedAt = %s, want roughly now", msg.ProducedAt)
	}
	// Microsecond truncation, because that is what a DATETIME(6) keeps and a
	// timestamp that changes when it is written down is one two logs disagree
	// about.
	if msg.ProducedAt != msg.ProducedAt.Truncate(time.Microsecond) {
		t.Errorf("ProducedAt = %s, want microsecond precision", msg.ProducedAt)
	}
}

func TestDecodeRoundTrip(t *testing.T) {
	want := NewDocumentMessage(id.New())
	b, err := Encode(want)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	got, err := Decode[DocumentMessage](b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.DocumentID != want.DocumentID || got.MessageID != want.MessageID {
		t.Errorf("Decode = %+v, want %+v", got, want)
	}
	if !got.ProducedAt.Equal(want.ProducedAt) {
		t.Errorf("ProducedAt = %s, want %s", got.ProducedAt, want.ProducedAt)
	}
	if got.EntityID() != want.DocumentID {
		t.Errorf("EntityID() = %q, want %q", got.EntityID(), want.DocumentID)
	}
}

func TestDecodeIgnoresUnknownFields(t *testing.T) {
	// A later version adding a field must not turn every older consumer into a
	// poison-message loop.
	body := `{"schema_version":1,"message_id":"` + id.New() + `","produced_at":"2026-09-20T22:04:11.482913Z",` +
		`"document_id":"` + id.New() + `","priority":"high"}`

	if _, err := Decode[DocumentMessage]([]byte(body)); err != nil {
		t.Fatalf("Decode with an unknown field: %v", err)
	}
}

func TestDecodeRejectsAnUnknownSchemaVersion(t *testing.T) {
	documentID := id.New()
	body := `{"schema_version":99,"message_id":"` + id.New() + `","produced_at":"2026-09-20T22:04:11.482913Z",` +
		`"document_id":"` + documentID + `"}`

	msg, err := Decode[DocumentMessage]([]byte(body))
	if !errors.Is(err, ErrUnknownSchemaVersion) {
		t.Fatalf("Decode error = %v, want ErrUnknownSchemaVersion", err)
	}
	// The value comes back anyway: the JSON parsed, so the ID is trustworthy,
	// and the handler needs it to record the failure against the right row.
	if msg.DocumentID != documentID {
		t.Errorf("DocumentID = %q, want %q: the handler cannot mark a row it was not told about",
			msg.DocumentID, documentID)
	}
}

func TestDecodeRejectsMalformedAndEmptyMessages(t *testing.T) {
	cases := map[string]string{
		"not json":       `{"schema_version":`,
		"wrong type":     `{"schema_version":"one","document_id":"` + id.New() + `"}`,
		"no document id": `{"schema_version":1,"message_id":"` + id.New() + `","produced_at":"2026-09-20T22:04:11Z"}`,
		"not a ulid":     `{"schema_version":1,"document_id":"nope"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			msg, err := Decode[DocumentMessage]([]byte(body))
			if !errors.Is(err, ErrInvalidMessage) {
				t.Fatalf("Decode error = %v, want ErrInvalidMessage", err)
			}
			if msg != (DocumentMessage{}) {
				t.Errorf("Decode returned %+v alongside the error, want the zero value", msg)
			}
		})
	}
}

func TestRunMessageUsesRunID(t *testing.T) {
	runID := id.New()
	b, err := Encode(NewRunMessage(runID))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if !strings.Contains(string(b), `"run_id"`) {
		t.Errorf("encoded run message has no run_id: %s", b)
	}
	got, err := Decode[RunMessage](b)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.EntityID() != runID {
		t.Errorf("EntityID() = %q, want %q", got.EntityID(), runID)
	}
}
