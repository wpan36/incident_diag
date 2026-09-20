package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/wpan36/incident_diag/internal/httpx"
)

// Document statuses. The transitions between them are defined by the methods
// below, not by callers assigning these values.
const (
	DocumentPending    = "PENDING"
	DocumentProcessing = "PROCESSING"
	DocumentReady      = "READY"
	DocumentFailed     = "FAILED"
)

// Document formats, derived from the uploaded file's extension rather than
// claimed by the client: the ingestion parser has to trust this value.
const (
	FormatMarkdown = "markdown"
	FormatText     = "text"
)

// Document types, supplied by the uploader.
const (
	DocumentTypeRunbook    = "runbook"
	DocumentTypePostmortem = "postmortem"
	DocumentTypeServiceDoc = "service_doc"
)

// Document is one uploaded knowledge file and its ingestion state.
type Document struct {
	ID       string
	Filename string
	// StoragePath is relative to the configured storage root, so the row stays
	// valid whatever the host layout is. It is never returned by the API.
	StoragePath   string
	Format        string
	SizeBytes     int64
	ContentSHA256 string
	Service       *string
	DocumentType  string
	Status        string
	FailureReason *string
	ChunkCount    int
	// Attempts and ProcessingStartedAt are ingestion bookkeeping, used by the
	// lease reclaim S2 will add. They are never returned by the API.
	Attempts            int
	ProcessingStartedAt *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// NewDocument is the caller-supplied part of a document. The file is already on
// disk by the time this is inserted.
type NewDocument struct {
	// ID is supplied by the caller, unlike every other entity here, because
	// the file on disk is stored under a directory named after it and has to
	// be written before the row exists. Generating a second identifier here
	// would leave storage_path pointing at a directory no document is called.
	ID            string
	Filename      string
	StoragePath   string
	Format        string
	SizeBytes     int64
	ContentSHA256 string
	Service       *string
	DocumentType  string
}

// DocumentFilter narrows a listing. An empty field means "do not filter on it".
type DocumentFilter struct {
	Service      string
	DocumentType string
}

const documentColumns = `id, filename, storage_path, format, size_bytes, content_sha256,
	service, document_type, status, failure_reason, chunk_count, attempts,
	processing_started_at, created_at, updated_at`

func scanDocument(s scanner) (Document, error) {
	var d Document
	err := s.Scan(&d.ID, &d.Filename, &d.StoragePath, &d.Format, &d.SizeBytes, &d.ContentSHA256,
		&d.Service, &d.DocumentType, &d.Status, &d.FailureReason, &d.ChunkCount, &d.Attempts,
		&d.ProcessingStartedAt, &d.CreatedAt, &d.UpdatedAt)
	return d, err
}

// CreateDocument inserts a document in PENDING and returns it as stored.
func (s *Store) CreateDocument(ctx context.Context, in NewDocument) (Document, error) {
	if in.ID == "" {
		return Document{}, httpx.Internal(errors.New("store: NewDocument.ID is empty"),
			"internal server error")
	}

	d := Document{
		ID:            in.ID,
		Filename:      in.Filename,
		StoragePath:   in.StoragePath,
		Format:        in.Format,
		SizeBytes:     in.SizeBytes,
		ContentSHA256: in.ContentSHA256,
		Service:       in.Service,
		DocumentType:  in.DocumentType,
		Status:        DocumentPending,
		CreatedAt:     now(),
	}
	d.UpdatedAt = d.CreatedAt

	const q = `INSERT INTO documents
	           (id, filename, storage_path, format, size_bytes, content_sha256,
	            service, document_type, status, created_at, updated_at)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q, d.ID, d.Filename, d.StoragePath, d.Format, d.SizeBytes,
		d.ContentSHA256, d.Service, d.DocumentType, d.Status, d.CreatedAt, d.UpdatedAt)
	if err != nil {
		return Document{}, dbError(err, "insert document")
	}
	return d, nil
}

// GetDocument returns the document with the given id, or a not-found error.
func (s *Store) GetDocument(ctx context.Context, documentID string) (Document, error) {
	const q = `SELECT ` + documentColumns + ` FROM documents WHERE id = ?`
	d, err := scanDocument(s.db.QueryRowContext(ctx, q, documentID))
	if errors.Is(err, sql.ErrNoRows) {
		return Document{}, httpx.NotFoundErr(err, "document %s not found", documentID)
	}
	if err != nil {
		return Document{}, dbError(err, "select document")
	}
	return d, nil
}

// ListDocuments returns one page of documents, newest first, narrowed by f.
func (s *Store) ListDocuments(ctx context.Context, f DocumentFilter, p PageParams) (Page[Document], error) {
	p = p.normalize()

	var c conditions
	if f.Service != "" {
		c.add("service = ?", f.Service)
	}
	if f.DocumentType != "" {
		c.add("document_type = ?", f.DocumentType)
	}
	c.cursorBefore(p.Cursor)

	q := `SELECT ` + documentColumns + ` FROM documents` + c.where() + ` ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(c.args, p.fetchLimit())...)
	if err != nil {
		return Page[Document]{}, dbError(err, "list documents")
	}
	defer rows.Close()

	var out []Document
	for rows.Next() {
		d, err := scanDocument(rows)
		if err != nil {
			return Page[Document]{}, dbError(err, "scan document")
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return Page[Document]{}, dbError(err, "list documents")
	}
	return paginate(out, p, func(d Document) string { return d.ID }), nil
}

// ClaimDocument moves a document into PROCESSING and reports whether it did.
//
// This is the claim half of the ingestion state machine: PENDING -> PROCESSING,
// and FAILED -> PROCESSING when a failed document is re-enqueued. The condition
// is in the UPDATE rather than in a preceding SELECT, which is what makes the
// consumer idempotent: false means another delivery of the same message already
// claimed this work, and the worker stops.
func (s *Store) ClaimDocument(ctx context.Context, documentID string) (bool, error) {
	const q = `UPDATE documents
	              SET status = ?, attempts = attempts + 1,
	                  processing_started_at = ?, updated_at = ?
	            WHERE id = ? AND status IN (?, ?)`
	t := now()
	res, err := s.db.ExecContext(ctx, q, DocumentProcessing, t, t, documentID, DocumentPending, DocumentFailed)
	if err != nil {
		return false, dbError(err, "claim document")
	}
	return changed(res, "claim document")
}

// MarkDocumentReady completes ingestion, recording how many chunks were
// indexed. It is conditional on PROCESSING so a late duplicate cannot overwrite
// a finished row.
func (s *Store) MarkDocumentReady(ctx context.Context, documentID string, chunkCount int) (bool, error) {
	const q = `UPDATE documents
	              SET status = ?, chunk_count = ?, failure_reason = NULL, updated_at = ?
	            WHERE id = ? AND status = ?`
	res, err := s.db.ExecContext(ctx, q, DocumentReady, chunkCount, now(), documentID, DocumentProcessing)
	if err != nil {
		return false, dbError(err, "mark document ready")
	}
	return changed(res, "mark document ready")
}

// MarkDocumentFailed records why ingestion failed, leaving the document
// re-enqueueable. Like the ready transition it is conditional on PROCESSING.
func (s *Store) MarkDocumentFailed(ctx context.Context, documentID, reason string) (bool, error) {
	const q = `UPDATE documents
	              SET status = ?, failure_reason = ?, updated_at = ?
	            WHERE id = ? AND status = ?`
	res, err := s.db.ExecContext(ctx, q, DocumentFailed, reason, now(), documentID, DocumentProcessing)
	if err != nil {
		return false, dbError(err, "mark document failed")
	}
	return changed(res, "mark document failed")
}
