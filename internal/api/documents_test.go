package api

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// uploadPart describes one part of a multipart upload under construction.
type uploadPart struct {
	field    string
	filename string // non-empty makes it a file part
	content  string
}

func buildUpload(t *testing.T, parts ...uploadPart) (body *bytes.Buffer, contentType string) {
	t.Helper()
	body = &bytes.Buffer{}
	w := multipart.NewWriter(body)

	for _, p := range parts {
		var (
			field io.Writer
			err   error
		)
		if p.filename != "" {
			field, err = w.CreateFormFile(p.field, p.filename)
		} else {
			field, err = w.CreateFormField(p.field)
		}
		if err != nil {
			t.Fatalf("building the %s part: %v", p.field, err)
		}
		if _, err := io.WriteString(field, p.content); err != nil {
			t.Fatalf("writing the %s part: %v", p.field, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the multipart writer: %v", err)
	}
	return body, w.FormDataContentType()
}

func upload(t *testing.T, h http.Handler, parts ...uploadPart) (*httptest.ResponseRecorder, errorBody) {
	t.Helper()
	body, contentType := buildUpload(t, parts...)
	req := httptest.NewRequest(http.MethodPost, "/api/documents", body)
	req.Header.Set("Content-Type", contentType)
	return do(t, h, req)
}

// assertNothingStored checks the cleanup that every failure path owes: a
// request that is answered with an error must not leave a file behind.
func assertNothingStored(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading the storage root: %v", err)
	}
	for _, e := range entries {
		t.Errorf("the storage root still contains %q after a failed upload", e.Name())
	}
}

func TestUploadRequiresMultipart(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/documents", strings.NewReader(`{"file":"x"}`))
	req.Header.Set("Content-Type", "application/json")

	rec, body := do(t, testRouter(t), req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body.Error.Message != "request must be multipart/form-data" {
		t.Errorf("message = %q", body.Error.Message)
	}
}

func TestUploadRequiresAFileAndADocumentType(t *testing.T) {
	tests := []struct {
		name  string
		parts []uploadPart
		field string
		code  string
	}{
		{
			name:  "no file",
			parts: []uploadPart{{field: "document_type", content: "runbook"}},
			field: "file",
			code:  "required",
		},
		{
			name:  "no document_type",
			parts: []uploadPart{{field: "file", filename: "runbook.md", content: "# runbook"}},
			field: "document_type",
			code:  "required",
		},
		{
			name: "unknown document_type",
			parts: []uploadPart{
				{field: "file", filename: "runbook.md", content: "# runbook"},
				{field: "document_type", content: "diary"},
			},
			field: "document_type",
			code:  "invalid_value",
		},
		{
			name: "a format the parser cannot read",
			parts: []uploadPart{
				{field: "file", filename: "runbook.pdf", content: "%PDF"},
				{field: "document_type", content: "runbook"},
			},
			field: "file",
			code:  "invalid_format",
		},
		{
			name: "a filename carrying a path",
			parts: []uploadPart{
				{field: "file", filename: "../../etc/passwd", content: "root:x:0:0"},
				{field: "document_type", content: "runbook"},
			},
			field: "file",
			code:  "invalid_format",
		},
		{
			name: "a malformed service",
			parts: []uploadPart{
				{field: "file", filename: "runbook.md", content: "# runbook"},
				{field: "document_type", content: "runbook"},
				{field: "service", content: "Payment Service"},
			},
			field: "service",
			code:  "invalid_format",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, fs := testServer(t)
			rec, body := upload(t, h, tt.parts...)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if body.Error.Fields[tt.field] != tt.code {
				t.Fatalf("fields = %v, want %s: %s", body.Error.Fields, tt.field, tt.code)
			}
			assertNothingStored(t, fs.Root())
		})
	}
}

// TestUploadCleansUpWhenAFieldAfterTheFileIsWrong is the ordering case: parts
// arrive in the order the client sent them, so the file can be on disk before
// the field that invalidates the request has been seen.
func TestUploadCleansUpWhenAFieldAfterTheFileIsWrong(t *testing.T) {
	h, fs := testServer(t)

	rec, _ := upload(t, h,
		uploadPart{field: "file", filename: "runbook.md", content: "# runbook"},
		uploadPart{field: "document_type", content: "diary"},
	)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	assertNothingStored(t, fs.Root())
}

func TestUploadRejectsAFileOverTheLimitWith413(t *testing.T) {
	h, fs := testServer(t)

	rec, body := upload(t, h,
		uploadPart{field: "file", filename: "runbook.md", content: strings.Repeat("a", testMaxUploadBytes+1)},
		uploadPart{field: "document_type", content: "runbook"},
	)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	// The envelope has no distinct kind for "too large"; the status is the
	// deviation, the code is not.
	if body.Error.Code != "invalid" {
		t.Errorf("code = %q, want invalid", body.Error.Code)
	}
	assertNothingStored(t, fs.Root())
}

func TestUploadRejectsAnOversizedEnvelopeWith413(t *testing.T) {
	h, fs := testServer(t)

	// An unknown part has to be drained to reach the next one, so without the
	// backstop on the whole body a client could stream one of these forever
	// while never sending a file at all.
	rec, _ := upload(t, h,
		uploadPart{field: "whatever", content: strings.Repeat("x", 2*multipartOverhead)},
		uploadPart{field: "document_type", content: "runbook"},
	)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	assertNothingStored(t, fs.Root())
}

func TestUploadReportsEveryProblemAtOnce(t *testing.T) {
	h, _ := testServer(t)

	_, body := upload(t, h,
		uploadPart{field: "file", filename: "runbook.pdf", content: "%PDF"},
		uploadPart{field: "document_type", content: "diary"},
		uploadPart{field: "service", content: "Payment Service"},
	)

	if len(body.Error.Fields) != 3 {
		t.Fatalf("fields = %v, want all three reported at once", body.Error.Fields)
	}
	if body.Error.Message != "request validation failed" {
		t.Errorf("message = %q, want the fixed multi-field message", body.Error.Message)
	}
}

func TestUploadIgnoresAnUnknownPart(t *testing.T) {
	h, _ := testServer(t)

	// No file, so this still fails — but on the missing file, not on the
	// unexpected part.
	_, body := upload(t, h,
		uploadPart{field: "document_type", content: "runbook"},
		uploadPart{field: "whatever", content: "a client library added this"},
	)
	if len(body.Error.Fields) != 1 || body.Error.Fields["file"] != "required" {
		t.Fatalf("fields = %v, want only file: required", body.Error.Fields)
	}
}

func TestDocumentListFiltersAreValidated(t *testing.T) {
	tests := []struct {
		query string
		field string
		code  string
	}{
		{"?service=Payment%20Service", "service", "invalid_format"},
		{"?document_type=diary", "document_type", "invalid_value"},
		{"?limit=0", "limit", "invalid_value"},
	}

	for _, tt := range tests {
		t.Run(tt.query, func(t *testing.T) {
			rec, body := do(t, testRouter(t), httptest.NewRequest(http.MethodGet, "/api/documents"+tt.query, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if body.Error.Fields[tt.field] != tt.code {
				t.Fatalf("fields = %v, want %s: %s", body.Error.Fields, tt.field, tt.code)
			}
		})
	}
}

func TestGetDocumentWithAMalformedIDIsNotFound(t *testing.T) {
	rec, body := do(t, testRouter(t), httptest.NewRequest(http.MethodGet, "/api/documents/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if body.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found", body.Error.Code)
	}
}

// TestDocumentListReportsPaginationAndFilterProblemsTogether is why the page
// parser accumulates into the caller's validation instead of returning its own
// error: answering with one of two problems makes the client fix it and get
// the other.
func TestDocumentListReportsPaginationAndFilterProblemsTogether(t *testing.T) {
	rec, body := do(t, testRouter(t),
		httptest.NewRequest(http.MethodGet, "/api/documents?limit=0&document_type=diary", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(body.Error.Fields) != 2 {
		t.Fatalf("fields = %v, want both limit and document_type", body.Error.Fields)
	}
}

func TestUploadRejectsAnEnvelopeThatOverflowsWhileTheFileIsStreaming(t *testing.T) {
	h, fs := testServer(t)

	// The file itself is comfortably under the per-file cap; it is the
	// unknown part in front of it that uses up the envelope. The overflow
	// therefore happens inside the streaming copy rather than between parts,
	// which used to surface as a 500 with "internal server error" — telling
	// the client this server was broken, and writing an ERROR log record, for
	// a request the client alone made too big.
	limit := int(fs.MaxBytes()) + multipartOverhead
	fileContent := strings.Repeat("#", int(fs.MaxBytes())-100)

	build := func(padding string) (*bytes.Buffer, string) {
		return buildUpload(t,
			uploadPart{field: "extra", content: padding},
			uploadPart{field: "document_type", content: "runbook"},
			uploadPart{field: "file", filename: "runbook.md", content: fileContent},
		)
	}

	// Measure the multipart framing with no padding, then pad so the file's
	// bytes land halfway across the limit. Boundaries vary between builds but
	// not in length, so the measurement holds for the real body.
	probe, _ := build("")
	framing := bytes.Index(probe.Bytes(), []byte(fileContent))
	if framing < 0 {
		t.Fatal("the file content is not in the body")
	}
	body, contentType := build(strings.Repeat("p", limit-framing-len(fileContent)/2))

	// Pin the arithmetic the test depends on: the file's bytes must straddle
	// the limit, so that the reader trips partway through them.
	start := bytes.Index(body.Bytes(), []byte(fileContent))
	if start >= limit || start+len(fileContent) <= limit {
		t.Fatalf("the file occupies bytes %d..%d, which does not straddle the %d-byte envelope",
			start, start+len(fileContent), limit)
	}
	if len(fileContent) > int(fs.MaxBytes()) {
		t.Fatal("the file is over the per-file cap, so this tests the wrong limit")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/documents", body)
	req.Header.Set("Content-Type", contentType)
	rec, errBody := do(t, h, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if errBody.Error.Code != "invalid" {
		t.Errorf("code = %q, want invalid", errBody.Error.Code)
	}
	assertNothingStored(t, fs.Root())
}

func TestUploadReportsAnOverlongFormValueAsAFieldError(t *testing.T) {
	for _, field := range []string{"service", "document_type"} {
		t.Run(field, func(t *testing.T) {
			h, fs := testServer(t)

			parts := []uploadPart{
				{field: "file", filename: "runbook.md", content: "# runbook\n"},
				{field: "document_type", content: "runbook"},
				{field: field, content: strings.Repeat("x", maxFormValueLen+1)},
			}
			rec, errBody := upload(t, h, parts...)

			// An over-long part is the client getting a field wrong, so it
			// belongs in fields with everything else. Reporting it as a body
			// that could not be parsed sent the client looking at its
			// multipart encoding instead of at the value it sent.
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if got := errBody.Error.Fields[field]; got != "too_long" {
				t.Errorf("fields[%s] = %q, want too_long", field, got)
			}
			assertNothingStored(t, fs.Root())
		})
	}
}
