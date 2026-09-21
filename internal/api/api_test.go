package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/wpan36/incident_diag/internal/files"
	"github.com/wpan36/incident_diag/internal/httpx"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/internal/mq"
	"github.com/wpan36/incident_diag/internal/store"
)

func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// testRouter builds the real router with no store behind it.
//
// Every request these tests make is rejected before a handler reaches the
// database, and a nil store makes that assumption loud: a test that started
// hitting MySQL would panic rather than quietly need infrastructure. File
// storage is real, because the upload tests write through it, but it points at
// a directory the test framework removes afterwards.
func testRouter(t *testing.T) http.Handler {
	t.Helper()
	h, _ := testServer(t)
	return h
}

// testServer also hands back the file storage, for the upload tests that have
// to check what did or did not reach disk.
func testServer(t *testing.T) (http.Handler, *files.Storage) {
	t.Helper()
	fs, err := files.New(t.TempDir(), testMaxUploadBytes)
	if err != nil {
		t.Fatalf("preparing file storage: %v", err)
	}
	return NewServer(Deps{Files: fs, Producer: &mq.FakeProducer{}, Logger: log.Discard()}).Router(), fs
}

// testServerWithProducer is testServer for the tests that care what was
// produced, which is only the upload path.
func testServerWithProducer(t *testing.T) (http.Handler, *files.Storage, *mq.FakeProducer) {
	t.Helper()
	fs, err := files.New(t.TempDir(), testMaxUploadBytes)
	if err != nil {
		t.Fatalf("preparing file storage: %v", err)
	}
	p := &mq.FakeProducer{}
	return NewServer(Deps{Files: fs, Producer: p, Logger: log.Discard()}).Router(), fs, p
}

// testMaxUploadBytes is small enough that a test can exceed it cheaply.
const testMaxUploadBytes = 1024

func do(t *testing.T, h http.Handler, req *http.Request) (*httptest.ResponseRecorder, errorBody) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var body errorBody
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("response body is not JSON: %v (%s)", err, rec.Body.String())
		}
	}
	return rec, body
}

func postIncident(t *testing.T, h http.Handler, body string) (*httptest.ResponseRecorder, errorBody) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/incidents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return do(t, h, req)
}

// --- timestamps -------------------------------------------------------------

func TestTimeRendersFixedMicrosecondPrecision(t *testing.T) {
	tests := []struct {
		name string
		in   time.Time
		want string
	}{
		{
			// The case Go's default RFC3339Nano gets wrong: it would trim the
			// zeros and emit "2026-09-20T10:11:12Z".
			name: "a whole second still shows six digits",
			in:   time.Date(2026, 9, 20, 10, 11, 12, 0, time.UTC),
			want: `"2026-09-20T10:11:12.000000Z"`,
		},
		{
			name: "microseconds are kept",
			in:   time.Date(2026, 9, 20, 10, 11, 12, 345678000, time.UTC),
			want: `"2026-09-20T10:11:12.345678Z"`,
		},
		{
			name: "a non-UTC instant is converted, not relabelled",
			in:   time.Date(2026, 9, 20, 10, 11, 12, 0, time.FixedZone("UTC+2", 2*60*60)),
			want: `"2026-09-20T08:11:12.000000Z"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(NewTime(tt.in))
			if err != nil {
				t.Fatalf("marshalling: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("got %s, want %s", got, tt.want)
			}
		})
	}
}

func TestNullTimeMarshalsAsNull(t *testing.T) {
	body, err := json.Marshal(struct {
		At *Time `json:"at"`
	}{At: NullTime(nil)})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if string(body) != `{"at":null}` {
		t.Fatalf("got %s, want an explicit null", body)
	}
}

// --- the error envelope -----------------------------------------------------

func TestValidationReportsEveryOffendingFieldInOneResponse(t *testing.T) {
	rec, body := postIncident(t, testRouter(t), `{
		"title": "",
		"description": "`+strings.Repeat("x", maxDescriptionLen+1)+`",
		"service": "Payment Service"
	}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body.Error.Code != "invalid" {
		t.Errorf("code = %q, want invalid", body.Error.Code)
	}
	// More than one field is at fault, so the message is fixed and the detail
	// lives in fields rather than in a sentence no client can parse.
	if body.Error.Message != "request validation failed" {
		t.Errorf("message = %q, want the fixed multi-field message", body.Error.Message)
	}
	want := map[string]string{
		"title":       "required",
		"description": "too_long",
		"service":     "invalid_format",
	}
	if len(body.Error.Fields) != len(want) {
		t.Fatalf("fields = %v, want all three reported at once", body.Error.Fields)
	}
	for field, code := range want {
		if body.Error.Fields[field] != code {
			t.Errorf("fields[%s] = %q, want %q", field, body.Error.Fields[field], code)
		}
	}
	if body.Error.RequestID == "" {
		t.Error("the error body carries no request_id")
	}
}

func TestASingleFieldFailureDescribesItself(t *testing.T) {
	_, body := postIncident(t, testRouter(t), `{"title": "   "}`)

	if body.Error.Message != "title is required" {
		t.Errorf("message = %q, want it to describe the one offending field", body.Error.Message)
	}
	if body.Error.Fields["title"] != "required" {
		t.Errorf("fields = %v, want title: required", body.Error.Fields)
	}
	// A title of only whitespace is required, not a three-character title.
	if len(body.Error.Fields) != 1 {
		t.Errorf("fields = %v, want only title", body.Error.Fields)
	}
}

func TestErrorBodyHasNoFieldsKeyWhenItIsNotAValidationError(t *testing.T) {
	rec := httptest.NewRecorder()
	testRouter(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "fields") {
		t.Fatalf("body = %s, want no fields key", rec.Body.String())
	}
}

func TestMalformedJSONIsRejected(t *testing.T) {
	rec, body := postIncident(t, testRouter(t), `{"title": `)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body.Error.Message != "request body is not valid JSON" {
		t.Errorf("message = %q", body.Error.Message)
	}
}

func TestAWronglyTypedFieldNamesItself(t *testing.T) {
	rec, body := postIncident(t, testRouter(t), `{"title": 7}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if body.Error.Fields["title"] != "invalid_type" {
		t.Fatalf("fields = %v, want title: invalid_type", body.Error.Fields)
	}
}

func TestGetIncidentWithAMalformedIDIsNotFound(t *testing.T) {
	rec, body := do(t, testRouter(t), httptest.NewRequest(http.MethodGet, "/api/incidents/not-an-id", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if body.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found", body.Error.Code)
	}
}

// --- pagination parameters --------------------------------------------------

func TestPaginationParametersAreRejectedNotCorrected(t *testing.T) {
	tests := []struct {
		name  string
		query string
		field string
		code  string
	}{
		{"not an integer", "?limit=twenty", "limit", "invalid_type"},
		{"below one", "?limit=0", "limit", "invalid_value"},
		{"negative", "?limit=-5", "limit", "invalid_value"},
		// Clamping this to 100 would let the client believe it had received a
		// complete result.
		{"above the maximum", "?limit=1000", "limit", "invalid_value"},
		{"empty", "?limit=", "limit", "invalid_type"},
		{"cursor is not a ulid", "?cursor=abc", "cursor", "invalid_format"},
		{"cursor is the wrong length", "?cursor=01JBQ8K3M7VXFZ2N9WQYRT4HC", "cursor", "invalid_format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, body := do(t, testRouter(t), httptest.NewRequest(http.MethodGet, "/api/incidents"+tt.query, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if body.Error.Fields[tt.field] != tt.code {
				t.Fatalf("fields = %v, want %s: %s", body.Error.Fields, tt.field, tt.code)
			}
		})
	}
}

func TestBothPaginationParametersAreReportedTogether(t *testing.T) {
	_, body := do(t, testRouter(t), httptest.NewRequest(http.MethodGet, "/api/incidents?limit=0&cursor=abc", nil))
	if len(body.Error.Fields) != 2 {
		t.Fatalf("fields = %v, want both reported at once", body.Error.Fields)
	}
	if body.Error.Message != "request validation failed" {
		t.Errorf("message = %q, want the fixed multi-field message", body.Error.Message)
	}
}

// --- request identifiers ----------------------------------------------------

func TestRequestIDIsGeneratedAndEchoed(t *testing.T) {
	rec := httptest.NewRecorder()
	testRouter(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	rid := rec.Header().Get(HeaderRequestID)
	if rid == "" {
		t.Fatal("no X-Request-ID on the response")
	}
	if !validRequestID(rid) {
		t.Fatalf("the generated id %q does not satisfy our own rule", rid)
	}
}

func TestAnAcceptableInboundRequestIDIsHonoured(t *testing.T) {
	const want = "trace-from-the-gateway_42"

	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	req.Header.Set(HeaderRequestID, want)
	rec, body := do(t, testRouter(t), req)

	if got := rec.Header().Get(HeaderRequestID); got != want {
		t.Errorf("response header = %q, want the inbound id", got)
	}
	if body.Error.RequestID != want {
		t.Errorf("error body request_id = %q, want the inbound id", body.Error.RequestID)
	}
}

func TestAnUnacceptableInboundRequestIDIsReplaced(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"too long", strings.Repeat("a", maxRequestIDLen+1)},
		// This value is written into every log record for the request, and
		// each record is one line of JSON: echoing a newline hands a client the
		// ability to forge one.
		{"carries a newline", "abc\ndef"},
		{"carries a carriage return", "abc\rdef"},
		{"carries a quote", `abc"def`},
		{"carries a space", "abc def"},
		{"carries a control character", "abc\x00def"},
		{"empty", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/nope", nil)
			req.Header.Set(HeaderRequestID, tt.in)
			rec, body := do(t, testRouter(t), req)

			got := rec.Header().Get(HeaderRequestID)
			if got == tt.in {
				t.Fatalf("the unacceptable id %q was echoed", tt.in)
			}
			if !validRequestID(got) {
				t.Fatalf("the replacement %q is not acceptable either", got)
			}
			if body.Error.RequestID != got {
				t.Errorf("body request_id = %q, header = %q; they must match", body.Error.RequestID, got)
			}
		})
	}
}

// --- health -----------------------------------------------------------------

func TestHealthzTouchesNothing(t *testing.T) {
	// The store is nil, so a liveness probe that consulted it would panic.
	rec := httptest.NewRecorder()
	testRouter(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if string(body) != `{"status":"ok"}` {
		t.Fatalf("body = %s", body)
	}
}

// --- response shapes --------------------------------------------------------

func TestAnEmptyListIsAnArray(t *testing.T) {
	body, err := json.Marshal(newIncidentList(store.Page[store.Incident]{}))
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if string(body) != `{"items":[]}` {
		t.Fatalf("body = %s, want items to be [] and next_cursor omitted", body)
	}
}

func TestANullableFieldRendersAsAnExplicitNull(t *testing.T) {
	body, err := json.Marshal(Incident{ID: "01JBQ8K3M7VXFZ2N9WQYRT4HCD", Title: "t"})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if !strings.Contains(string(body), `"service":null`) {
		t.Fatalf("body = %s, want an explicit null service", body)
	}
}

// --- request size limits ----------------------------------------------------

func TestAnOversizedJSONBodyIsRejectedBeforeItIsDecoded(t *testing.T) {
	h := testRouter(t)

	// Valid JSON, just far too much of it. Without the bound the decoder reads
	// the whole thing into memory and only then finds that description is too
	// long, which is a cost the client chooses and the server pays.
	body := `{"title":"ok","description":"` + strings.Repeat("a", maxJSONBodyBytes) + `"}`
	rec, errBody := postIncident(t, h, body)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if errBody.Error.Code != "invalid" {
		t.Errorf("code = %q, want invalid", errBody.Error.Code)
	}
	if errBody.Error.RequestID == "" {
		t.Error("the error body carries no request_id")
	}
}

func TestABodyAtTheLimitIsStillDecoded(t *testing.T) {
	h := testRouter(t)

	// Under the bound, so it reaches validation and is rejected on its merits
	// — for the description's length, with a field, not for its size.
	body := `{"title":"ok","description":"` + strings.Repeat("a", maxDescriptionLen+1) + `"}`
	rec, errBody := postIncident(t, h, body)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := errBody.Error.Fields["description"]; got != "too_long" {
		t.Errorf("fields[description] = %q, want too_long", got)
	}
}

// --- lengths are measured in characters -------------------------------------

func TestTextLimitsCountCharactersNotBytes(t *testing.T) {
	// A title of maxTitleLen Chinese characters is three times that many bytes
	// and fits VARCHAR(255) exactly, because MySQL counts characters too.
	// Counting bytes here used to reject it — while telling the client it was
	// longer than 255 characters, which it was not.
	var v validation
	v.requiredText("title", strings.Repeat("测", maxTitleLen), maxTitleLen)
	if err := v.err(); err != nil {
		t.Errorf("a %d-character title was rejected: %v", maxTitleLen, err)
	}

	var tooLong validation
	tooLong.requiredText("title", strings.Repeat("测", maxTitleLen+1), maxTitleLen)
	if err := tooLong.err(); err == nil {
		t.Error("a title one character over the limit was accepted")
	} else if got := httpx.FieldsOf(err)["title"]; got != "too_long" {
		t.Errorf("fields[title] = %q, want too_long", got)
	}

	var desc validation
	desc.optionalText("description", strings.Repeat("测", maxDescriptionLen), maxDescriptionLen)
	if err := desc.err(); err != nil {
		t.Errorf("a %d-character description was rejected: %v", maxDescriptionLen, err)
	}
}

func TestFilenameLengthCountsCharacters(t *testing.T) {
	var v validation
	name, format, ok := acceptFilename(&v, strings.Repeat("测", maxFilenameLen-3)+".md")
	if !ok {
		t.Fatalf("a %d-character filename was rejected: %v", maxFilenameLen, v.err())
	}
	if format != "markdown" {
		t.Errorf("format = %q, want markdown", format)
	}
	if utf8.RuneCountInString(name) != maxFilenameLen {
		t.Errorf("name is %d characters, want %d", utf8.RuneCountInString(name), maxFilenameLen)
	}

	var tooLong validation
	if _, _, ok := acceptFilename(&tooLong, strings.Repeat("测", maxFilenameLen-2)+".md"); ok {
		t.Error("a filename one character over the limit was accepted")
	}
}
