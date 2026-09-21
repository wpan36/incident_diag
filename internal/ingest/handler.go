// Package ingest consumes documents.ingest.v1 and drives one document through
// the ingestion state machine: claim, parse, chunk, embed, index, and a
// terminal write.
//
// The frame around the work — decode, claim, terminal write — is where
// idempotency lives, which is why it was built first and separately. The claim
// is a conditional UPDATE, so a redelivery finds the work already taken; the
// terminal writes are conditional on PROCESSING, so a late duplicate cannot
// overwrite a finished row.
//
// The parser and the chunker in this package are pure functions: text in,
// chunks out, no I/O and no clock. They carry most of this package's tests.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/wpan36/incident_diag/internal/embed"
	"github.com/wpan36/incident_diag/internal/files"
	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/search"
	"github.com/wpan36/incident_diag/internal/store"
)

// terminalTimeout bounds the writes that record how a document ended.
//
// They run on a context detached from the document's deadline, because the
// most important thing a handler that has just run out of time can do is say
// so. A cancelled write would leave the row PROCESSING and nothing but the
// lease to rescue it.
const terminalTimeout = 10 * time.Second

// maxFailureReason bounds what goes into the failure_reason column, which is
// read by a human through GET /api/documents.
const maxFailureReason = 1000

// Deps is everything the handler needs. It is a struct rather than eight
// positional parameters, because half of them are durations and a caller that
// swaps two of those compiles.
type Deps struct {
	Store    *store.Store
	Files    *files.Storage
	Embedder embed.Embedder
	Search   *search.Client

	// Lease must be the same value the reconciler uses, or the sweep would
	// re-enqueue rows the claim then refuses.
	Lease time.Duration

	// DocumentTimeout is the deadline on one document, applied the moment the
	// claim succeeds. config.LoadReconcile has already checked that it is
	// shorter than Lease.
	DocumentTimeout time.Duration

	Chunking ChunkOptions
	Logger   *slog.Logger
}

// Handler processes one documents.ingest.v1 record.
type Handler struct {
	deps Deps
}

// NewHandler builds the handler.
func NewHandler(deps Deps) *Handler { return &Handler{deps: deps} }

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
		h.deps.Logger.ErrorContext(ctx, "message has an unknown schema version",
			"document_id", msg.DocumentID, "offset", rec.Offset, "error", err)
		return h.failWithoutWork(ctx, msg.DocumentID, fmt.Sprintf("unrecognized message: %v", err))

	case err != nil:
		// Nothing in the record can be trusted, so there is no row to mark. The
		// row this message was about is still PENDING, and the reconciler will
		// produce a well-formed message for it.
		h.deps.Logger.ErrorContext(ctx, "message could not be decoded",
			"topic", rec.Topic, "partition", rec.Partition, "offset", rec.Offset, "error", err)
		return nil
	}

	claimed, err := h.deps.Store.ClaimDocument(ctx, msg.DocumentID, h.deps.Lease)
	if err != nil {
		return fmt.Errorf("claim document %s: %w", msg.DocumentID, err)
	}
	if !claimed {
		return h.explainRefusedClaim(ctx, msg.DocumentID)
	}
	return h.process(ctx, msg.DocumentID)
}

// process does the work of one claimed document, under its own deadline.
//
// Without that deadline the worst case is not bounded at all: a document of
// CHUNK_MAX_PER_DOCUMENT chunks is dozens of embedding requests, each retried
// with backoff, which together can run for well over an hour. Two things break
// when they do — the lease stops meaning anything, and a member that blocks
// through a rebalance is evicted and its message redelivered.
func (h *Handler) process(ctx context.Context, documentID string) error {
	ctx, cancel := context.WithTimeout(ctx, h.deps.DocumentTimeout)
	defer cancel()

	started := time.Now()
	h.deps.Logger.InfoContext(ctx, "claimed document", "document_id", documentID)

	chunkCount, err := h.ingest(ctx, documentID)
	if err != nil {
		h.deps.Logger.ErrorContext(ctx, "ingestion failed",
			"document_id", documentID, "duration", time.Since(started), "error", err)
		return h.markFailed(ctx, documentID, err.Error())
	}

	// The terminal write is detached from the document's deadline for the same
	// reason the failure path is: having done the work, the row must say so.
	writeCtx, cancelWrite := context.WithTimeout(context.WithoutCancel(ctx), terminalTimeout)
	defer cancelWrite()

	updated, err := h.deps.Store.MarkDocumentReady(writeCtx, documentID, chunkCount)
	if err != nil {
		return fmt.Errorf("mark document %s ready: %w", documentID, err)
	}
	if !updated {
		// Per the store's contract, false means the work was already done by
		// another delivery, not that anything failed.
		h.deps.Logger.DebugContext(ctx, "document was already finished by another delivery",
			"document_id", documentID)
		return nil
	}

	h.deps.Logger.InfoContext(ctx, "document ready",
		"document_id", documentID, "chunks", chunkCount, "duration", time.Since(started))
	return nil
}

// ingest is parse, chunk, embed, delete, index — the order being the thing that
// makes a document either fully indexed or not indexed at all.
//
// Embedding comes before the delete, and that is load-bearing: a document that
// is already READY and healthy keeps its chunks through a failed re-ingest,
// because nothing is removed until every vector is in hand. Deleting first
// would mean one provider 400 costs retrieval a document it already had.
func (h *Handler) ingest(ctx context.Context, documentID string) (int, error) {
	doc, err := h.deps.Store.GetDocument(ctx, documentID)
	if err != nil {
		return 0, fmt.Errorf("read the document row: %w", err)
	}

	raw, err := h.read(doc)
	if err != nil {
		return 0, err
	}

	chunks, err := ChunkDocument(doc.Format, raw, h.deps.Chunking)
	if err != nil {
		return 0, err
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
		if c.Tokens > h.deps.Chunking.TargetTokens {
			// Never split, so it is expected rather than exceptional — but it
			// is the one place where a chunk stops being the size the design
			// intended, so it is visible.
			h.deps.Logger.WarnContext(ctx, "chunk is over the token target",
				"document_id", doc.ID, "chunk_id", search.ChunkID(doc.ID, c.Index),
				"estimated_tokens", c.Tokens, "target_tokens", h.deps.Chunking.TargetTokens)
		}
	}

	vectors, err := h.deps.Embedder.Embed(ctx, texts)
	if err != nil {
		return 0, err
	}
	if len(vectors) != len(chunks) {
		return 0, fmt.Errorf("the embedder returned %d vectors for %d chunks", len(vectors), len(chunks))
	}

	indexed := make([]search.Chunk, len(chunks))
	now := time.Now().UTC()
	for i, c := range chunks {
		indexed[i] = search.Chunk{
			DocumentID:   doc.ID,
			ChunkID:      search.ChunkID(doc.ID, c.Index),
			ChunkIndex:   c.Index,
			Service:      doc.Service,
			DocumentType: doc.DocumentType,
			Source:       doc.Filename,
			HeadingPath:  c.HeadingPath,
			Content:      c.Content,
			Embedding:    vectors[i],
			IndexedAt:    now,
		}
	}

	if err := h.deps.Search.DeleteByDocument(ctx, doc.ID); err != nil {
		return 0, err
	}
	if err := h.deps.Search.IndexChunks(ctx, indexed); err != nil {
		// A document indexed at sixty percent is the hardest kind of bad state
		// to notice, because it shows up only as a retrieval score that is
		// quietly worse than it should be. This is why the bulk requests wait
		// for a refresh: a delete issued against an unrefreshed bulk would
		// match nothing.
		h.cleanup(ctx, doc.ID)
		return 0, err
	}
	return len(chunks), nil
}

// read returns the stored file's bytes.
//
// The limit is the upload limit the file was accepted under, plus one byte so
// that a file which has grown since is detected rather than silently truncated
// into chunks that do not match the document.
func (h *Handler) read(doc store.Document) ([]byte, error) {
	f, err := h.deps.Files.Open(doc.ID, doc.Filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	limit := h.deps.Files.MaxBytes()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read the stored file: %w", err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("the stored file is larger than the %d byte upload limit", limit)
	}
	return raw, nil
}

// cleanup removes whatever a failed index left behind, on a context detached
// from the document's deadline: the deadline expiring is one of the ways the
// index fails, and the chunks still have to go.
func (h *Handler) cleanup(ctx context.Context, documentID string) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalTimeout)
	defer cancel()

	if err := h.deps.Search.DeleteByDocument(cleanupCtx, documentID); err != nil {
		// Nothing better is available: the document is about to be marked
		// FAILED, and a second error here would hide the first. The next
		// attempt's leading delete is what finally clears it.
		h.deps.Logger.ErrorContext(ctx, "could not remove the chunks of a failed document",
			"document_id", documentID, "error", err)
	}
}

// markFailed records why ingestion failed. Both permanent and transient
// failures end here: S2 records that a FAILED row does not say which it was,
// and the reconciler re-enqueues both until the attempt limit is reached.
func (h *Handler) markFailed(ctx context.Context, documentID, reason string) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), terminalTimeout)
	defer cancel()

	if _, err := h.deps.Store.MarkDocumentFailed(writeCtx, documentID, truncate(reason, maxFailureReason)); err != nil {
		return fmt.Errorf("mark document %s failed: %w", documentID, err)
	}
	return nil
}

// failWithoutWork records a permanent failure against a document nothing has
// claimed yet. The claim has to happen first, because the terminal writes are
// conditional on PROCESSING — which is what stops a late duplicate from
// overwriting a finished row.
func (h *Handler) failWithoutWork(ctx context.Context, documentID, reason string) error {
	claimed, err := h.deps.Store.ClaimDocument(ctx, documentID, h.deps.Lease)
	if err != nil {
		return fmt.Errorf("claim document %s: %w", documentID, err)
	}
	if !claimed {
		return h.explainRefusedClaim(ctx, documentID)
	}
	return h.markFailed(ctx, documentID, reason)
}

// explainRefusedClaim turns a false claim into a log line that says which of
// the two possible reasons it was.
//
// A refused claim means either that another delivery already has the work — the
// ordinary, healthy case under at-least-once delivery — or that the message
// names a row that does not exist, which is a real problem and would otherwise
// be invisible. The extra read only happens on this path, which is the rare one.
func (h *Handler) explainRefusedClaim(ctx context.Context, documentID string) error {
	_, err := h.deps.Store.GetDocument(ctx, documentID)
	switch {
	case err == nil:
		h.deps.Logger.DebugContext(ctx, "document already claimed, skipping",
			"document_id", documentID)
		return nil
	case httpx.KindOf(err) == httpx.KindNotFound:
		h.deps.Logger.ErrorContext(ctx, "message names a document that does not exist",
			"document_id", documentID)
		return nil
	default:
		return fmt.Errorf("look up document %s: %w", documentID, err)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + "…"
}
