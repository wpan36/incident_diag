package api

import (
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/files"
	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/id"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/store"
)

// Upload limits.
const (
	// maxFilenameLen matches the documents.filename column.
	maxFilenameLen = 255

	// maxFormValueLen bounds the non-file parts. document_type and service are
	// both short, and a part that streams megabytes into a string is not an
	// upload this endpoint has to support.
	maxFormValueLen = 1024

	// multipartOverhead is how much the envelope around the file — boundaries,
	// part headers, the other two fields — is allowed to add on top of the file
	// limit itself. It is the backstop that stops a client streaming an
	// unbounded number of parts that are individually small.
	multipartOverhead = 1 << 20 // 1 MiB
)

// uploadFormats maps an accepted extension to the stored format.
//
// The format is derived rather than being a fourth form field: it is a property
// of the file, not a claim the client should be able to make about it, and the
// ingestion parser in M7 has to trust it.
var uploadFormats = map[string]string{
	".md":  store.FormatMarkdown,
	".txt": store.FormatText,
}

// documentTypes is the closed set the uploader chooses from.
//
// document_type is required although defaulting it would be friendlier to a
// curl smoke test, because a default produces a corpus of mislabelled documents
// that M11's metadata filter then cannot separate.
var documentTypes = map[string]bool{
	store.DocumentTypeRunbook:    true,
	store.DocumentTypePostmortem: true,
	store.DocumentTypeServiceDoc: true,
}

// uploadedDocument is what one pass over the multipart body produced.
type uploadedDocument struct {
	filename     string
	format       string
	saved        *files.Saved
	documentType string
	service      *string
}

func (s *Server) uploadDocument(c *gin.Context) {
	// A backstop on the whole request, distinct from the per-file limit
	// enforced while streaming. Without it a client could hold the connection
	// open with unbounded non-file parts.
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, s.deps.Files.MaxBytes()+multipartOverhead)

	reader, err := c.Request.MultipartReader()
	if err != nil {
		renderError(c, httpx.InvalidErr(err, "request must be multipart/form-data"))
		return
	}

	documentID := id.New()
	up, v, err := s.readUpload(reader, documentID)
	if err != nil {
		// The file, if any, is already gone: readUpload cleans up before
		// returning an error.
		if errors.Is(err, files.ErrTooLarge) {
			renderError(c, tooLarge("file must be at most %d bytes", s.deps.Files.MaxBytes()))
			return
		}
		renderError(c, err)
		return
	}

	if up.saved == nil {
		v.add("file", httpx.CodeRequired, "file is required")
	}
	if up.documentType == "" {
		v.add("document_type", httpx.CodeRequired, "document_type is required")
	}
	if err := v.err(); err != nil {
		s.removeUpload(c, documentID)
		renderError(c, err)
		return
	}

	// The row is inserted only after the file is safely on disk, so a crash
	// leaves an orphaned file rather than a row pointing at nothing.
	doc, err := s.deps.Store.CreateDocument(c.Request.Context(), store.NewDocument{
		ID:            documentID,
		Filename:      up.filename,
		StoragePath:   up.saved.Path,
		Format:        up.format,
		SizeBytes:     up.saved.Size,
		ContentSHA256: up.saved.SHA256,
		Service:       up.service,
		DocumentType:  up.documentType,
	})
	if err != nil {
		// A failed insert is not a crash, so the orphan is avoidable here.
		s.removeUpload(c, documentID)
		renderError(c, err)
		return
	}

	s.enqueueIngestion(c, doc.ID)
	renderCreated(c, "/api/documents/"+doc.ID, newDocument(doc))
}

// enqueueIngestion asks a worker to ingest the document that was just created.
//
// A produce that fails is logged and nothing else. The response is still 201,
// which looks wrong and is deliberate: the document genuinely was created, so
// 500 would be a lie, and a client that retried on 500 would upload a second
// copy of the same file. The row is PENDING, which is exactly the state the
// reconciler exists to find, so the only real cost of a failed produce is that
// ingestion starts a minute late.
//
// Being able to answer honestly here is the first thing the reconciler buys.
func (s *Server) enqueueIngestion(c *gin.Context, documentID string) {
	ctx := c.Request.Context()
	err := s.deps.Producer.Produce(ctx, mq.TopicDocumentsIngest, documentID, mq.NewDocumentMessage(documentID))
	if err == nil {
		return
	}
	s.deps.Logger.ErrorContext(ctx, "could not enqueue document for ingestion",
		"document_id", documentID, "error", err)
}

// readUpload makes one streaming pass over the multipart body.
//
// Parts arrive in whatever order the client sent them, so the file may be
// written before document_type has been seen. That is why the caller cleans up
// on a validation failure rather than this function trying to validate first.
func (s *Server) readUpload(reader *multipart.Reader, documentID string) (uploadedDocument, validation, error) {
	var up uploadedDocument
	var v validation

	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.deps.Files.Remove(documentID)
			return up, v, invalidBody(err)
		}

		switch part.FormName() {
		case "file":
			if up.saved != nil {
				v.add("file", httpx.CodeInvalidValue, "file must be sent once")
				part.Close()
				continue
			}
			name, format, ok := acceptFilename(&v, part.FileName())
			if !ok {
				part.Close()
				continue
			}
			saved, err := s.deps.Files.Save(documentID, name, part)
			part.Close()
			if err != nil {
				// Save reports the reader's failure as it found it, and the
				// reader here is the request body under MaxBytesReader.
				// Exceeding the envelope mid-file is the client sending too
				// much, not this server failing, and answering it with a 500
				// would both lie to the client and put an ERROR record in the
				// log for something no operator can act on. Anything else — a
				// full disk, a read-only volume — really is a server fault and
				// keeps its classification.
				if isTooLarge(err) {
					return up, v, files.ErrTooLarge
				}
				return up, v, err
			}
			up.filename, up.format, up.saved = name, format, &saved

		case "document_type":
			value, tooLong, err := readFormValue(part)
			if err != nil {
				s.deps.Files.Remove(documentID)
				return up, v, invalidBody(err)
			}
			if tooLong {
				v.add("document_type", httpx.CodeTooLong, formValueTooLong("document_type"))
				continue
			}
			if !documentTypes[value] {
				v.add("document_type", httpx.CodeInvalidValue,
					"document_type must be one of runbook, postmortem, service_doc")
				continue
			}
			up.documentType = value

		case "service":
			value, tooLong, err := readFormValue(part)
			if err != nil {
				s.deps.Files.Remove(documentID)
				return up, v, invalidBody(err)
			}
			if tooLong {
				v.add("service", httpx.CodeTooLong, formValueTooLong("service"))
				continue
			}
			up.service = v.optionalService("service", &value)

		default:
			// An unknown part is ignored rather than rejected: a client library
			// that adds one should not break, and nothing here depends on the
			// set of parts being exactly three.
			part.Close()
		}
	}
	return up, v, nil
}

// removeUpload deletes a file written for a request that then failed.
func (s *Server) removeUpload(c *gin.Context, documentID string) {
	if err := s.deps.Files.Remove(documentID); err != nil {
		s.deps.Logger.WarnContext(c.Request.Context(), "could not remove the file of a failed upload",
			"document_id", documentID, "error", err)
	}
}

// acceptFilename validates the uploaded name and derives the stored format.
func acceptFilename(v *validation, name string) (filename, format string, ok bool) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		v.add("file", httpx.CodeRequired, "file must have a filename")
		return "", "", false
	case utf8.RuneCountInString(name) > maxFilenameLen:
		// Characters, not bytes: the column is VARCHAR(255), which MySQL
		// counts in characters, and counting bytes here would reject a
		// perfectly storable name the moment it stopped being ASCII.
		v.add("file", httpx.CodeTooLong, "filename must be at most 255 characters")
		return "", "", false
	case name != filepath.Base(name) || strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, 0):
		// An upload is not allowed to influence where it lands.
		v.add("file", httpx.CodeInvalidFormat, "filename must not contain a path")
		return "", "", false
	}

	format, known := uploadFormats[strings.ToLower(filepath.Ext(name))]
	if !known {
		v.add("file", httpx.CodeInvalidFormat, "file must be a .md or .txt file")
		return "", "", false
	}
	return name, format, true
}

// readFormValue reads one bounded non-file part.
//
// An over-long value is reported through tooLong rather than as an error,
// because it is the client getting a field wrong and belongs in `fields` with
// every other rejected field. Returning it as an error made the response say
// the body could not be parsed, which sent the client looking at its multipart
// encoding instead of at the value it sent.
func readFormValue(part *multipart.Part) (value string, tooLong bool, err error) {
	defer part.Close()
	// One byte past the bound, so an over-long value is detected rather than
	// silently truncated into something that might happen to be valid.
	raw, err := io.ReadAll(io.LimitReader(part, maxFormValueLen+1))
	if err != nil {
		return "", false, err
	}
	if len(raw) > maxFormValueLen {
		return "", true, nil
	}
	return strings.TrimSpace(string(raw)), false, nil
}

// isTooLarge reports whether err is the request envelope's limit tripping.
//
// http.MaxBytesReader returns this however deep the reader it wraps is, so it
// surfaces both from reading a part and from streaming the file out of one.
func isTooLarge(err error) bool {
	var maxBytes *http.MaxBytesError
	return errors.As(err, &maxBytes)
}

// formValueTooLong is the message for a non-file part over maxFormValueLen. It
// says bytes because that is what the bound counts: the part is capped while
// it is still being read, before there is a string to count characters in.
func formValueTooLong(field string) string {
	return field + " must be at most " + itoa(maxFormValueLen) + " bytes"
}

// invalidBody classifies a failure to read the request body.
//
// The envelope limit means the client sent more than the request may carry,
// which is the same answer as an oversized file.
func invalidBody(err error) error {
	if isTooLarge(err) {
		return files.ErrTooLarge
	}
	return httpx.InvalidErr(err, "request body could not be read as multipart/form-data")
}

func (s *Server) listDocuments(c *gin.Context) {
	var v validation
	p := parsePageParams(c, &v)

	var f store.DocumentFilter
	if raw, ok := c.GetQuery("service"); ok {
		if service := v.optionalService("service", &raw); service != nil {
			f.Service = *service
		}
	}
	if raw, ok := c.GetQuery("document_type"); ok && raw != "" {
		if !documentTypes[raw] {
			v.add("document_type", httpx.CodeInvalidValue,
				"document_type must be one of runbook, postmortem, service_doc")
		} else {
			f.DocumentType = raw
		}
	}
	if err := v.err(); err != nil {
		renderError(c, err)
		return
	}

	page, err := s.deps.Store.ListDocuments(c.Request.Context(), f, p)
	if err != nil {
		renderError(c, err)
		return
	}
	c.JSON(http.StatusOK, newDocumentList(page))
}

func (s *Server) getDocument(c *gin.Context) {
	documentID := c.Param("id")
	if !validID(documentID) {
		renderError(c, httpx.NotFound("document %s not found", documentID))
		return
	}

	doc, err := s.deps.Store.GetDocument(c.Request.Context(), documentID)
	if err != nil {
		renderError(c, err)
		return
	}
	c.JSON(http.StatusOK, newDocument(doc))
}
