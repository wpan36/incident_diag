// Package ingest consumes documents.ingest.v1 and drives one document through
// the ingestion state machine.
//
// M6 delivers the frame: decode, claim, terminal write. The work between the
// claim and the terminal write — parse, chunk, embed, index — is M10, and until
// then the handler marks every document failed with a reason that says so. The
// frame is the part worth building first, because it is the part idempotency
// lives in.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/store"
)

// notImplemented is what the placeholder records. It reads as a failure to an
// operator looking at GET /api/documents, which is honest: M6 genuinely cannot
// ingest anything.
const notImplemented = "ingestion is not implemented yet (M10)"

// Handler processes one documents.ingest.v1 record.
type Handler struct {
	store  *store.Store
	lease  time.Duration
	logger *slog.Logger
}

// NewHandler builds the handler. The lease must be the same value the
// reconciler uses, or the sweep would re-enqueue rows the claim then refuses.
func NewHandler(st *store.Store, lease time.Duration, logger *slog.Logger) *Handler {
	return &Handler{store: st, lease: lease, logger: logger}
}

// Handle is an mq.Handler.
//
// It returns an error only when something outside this message is wrong — MySQL
// unreachable, most likely. Every outcome that is about the message itself is
// recorded in MySQL and returns nil, because the offset is going to be
// committed either way and a returned error would only add noise.
func (h *Handler) Handle(ctx context.Context, rec mq.Record) error {
	msg, err := mq.Decode[mq.DocumentMessage](rec.Value)

	switch {
	case errors.Is(err, mq.ErrUnknownSchemaVersion):
		// The JSON parsed, so the ID is trustworthy and the failure can be
		// recorded against the right row. Redelivering this message forever
		// cannot help: this build will never understand that version.
		h.logger.ErrorContext(ctx, "message has an unknown schema version",
			"document_id", msg.DocumentID, "offset", rec.Offset, "error", err)
		return h.fail(ctx, msg.DocumentID, fmt.Sprintf("unrecognized message: %v", err))

	case err != nil:
		// Nothing in the record can be trusted, so there is no row to mark. The
		// row this message was about is still PENDING, and the reconciler will
		// produce a well-formed message for it.
		h.logger.ErrorContext(ctx, "message could not be decoded",
			"topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset, "error", err)
		return nil
	}

	claimed, err := h.store.ClaimDocument(ctx, msg.DocumentID, h.lease)
	if err != nil {
		return fmt.Errorf("claim document %s: %w", msg.DocumentID, err)
	}
	if !claimed {
		return h.explainRefusedClaim(ctx, msg.DocumentID)
	}

	// Where M10's parse, chunk, embed and index will go. Marking the document
	// failed rather than ready is deliberate: a READY document with no chunks
	// would be a lie that the retrieval evaluation would later have to unpick.
	h.logger.InfoContext(ctx, "claimed document", "document_id", msg.DocumentID)
	if _, err := h.store.MarkDocumentFailed(ctx, msg.DocumentID, notImplemented); err != nil {
		return fmt.Errorf("mark document %s failed: %w", msg.DocumentID, err)
	}
	return nil
}

// fail records a permanent failure against a document that has not been claimed
// yet. The claim has to happen first, because the terminal writes are
// conditional on PROCESSING — which is what stops a late duplicate from
// overwriting a finished row.
func (h *Handler) fail(ctx context.Context, documentID, reason string) error {
	claimed, err := h.store.ClaimDocument(ctx, documentID, h.lease)
	if err != nil {
		return fmt.Errorf("claim document %s: %w", documentID, err)
	}
	if !claimed {
		return h.explainRefusedClaim(ctx, documentID)
	}
	if _, err := h.store.MarkDocumentFailed(ctx, documentID, reason); err != nil {
		return fmt.Errorf("mark document %s failed: %w", documentID, err)
	}
	return nil
}

// explainRefusedClaim turns a false claim into a log line that says which of
// the two possible reasons it was.
//
// A refused claim means either that another delivery already has the work — the
// ordinary, healthy case under at-least-once delivery — or that the message
// names a row that does not exist, which is a real problem and would otherwise
// be invisible. The extra read only happens on this path, which is the rare one.
func (h *Handler) explainRefusedClaim(ctx context.Context, documentID string) error {
	_, err := h.store.GetDocument(ctx, documentID)
	switch {
	case err == nil:
		h.logger.DebugContext(ctx, "document already claimed, skipping",
			"document_id", documentID)
		return nil
	case httpx.KindOf(err) == httpx.KindNotFound:
		h.logger.ErrorContext(ctx, "message names a document that does not exist",
			"document_id", documentID)
		return nil
	default:
		return fmt.Errorf("look up document %s: %w", documentID, err)
	}
}
