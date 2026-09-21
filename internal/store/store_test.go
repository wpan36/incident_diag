package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/httpx"
)

func TestPaginate(t *testing.T) {
	idOf := func(s string) string { return s }

	tests := []struct {
		name       string
		rows       []string
		limit      int
		wantItems  []string
		wantCursor string
	}{
		{
			name:      "no rows yields an empty slice, not nil",
			rows:      nil,
			limit:     20,
			wantItems: []string{},
		},
		{
			name:      "a partial page has no cursor",
			rows:      []string{"c", "b"},
			limit:     20,
			wantItems: []string{"c", "b"},
		},
		{
			// The case that matters: with limit+1 fetched and exactly limit
			// returned, this is the last page even though it is full. Inferring
			// the end from "fewer rows than limit" would hand the client a
			// cursor that fetches nothing.
			name:      "an exactly full last page has no cursor",
			rows:      []string{"c", "b"},
			limit:     2,
			wantItems: []string{"c", "b"},
		},
		{
			name:       "a full page with more behind it carries the last id",
			rows:       []string{"c", "b", "a"},
			limit:      2,
			wantItems:  []string{"c", "b"},
			wantCursor: "b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := paginate(tt.rows, PageParams{Limit: tt.limit}, idOf)
			if got.NextCursor != tt.wantCursor {
				t.Errorf("NextCursor = %q, want %q", got.NextCursor, tt.wantCursor)
			}
			if got.Items == nil {
				t.Fatal("Items is nil; the API renders an empty page as [], never null")
			}
			if fmt.Sprint(got.Items) != fmt.Sprint(tt.wantItems) {
				t.Errorf("Items = %v, want %v", got.Items, tt.wantItems)
			}
		})
	}
}

func TestFetchLimitAsksForOneExtraRow(t *testing.T) {
	if got := (PageParams{Limit: 20}).fetchLimit(); got != 21 {
		t.Fatalf("fetchLimit = %d, want 21", got)
	}
}

func TestConditionsBuildsOneWhereClause(t *testing.T) {
	var c conditions
	if c.where() != "" {
		t.Fatalf("empty conditions produced %q, want no WHERE clause", c.where())
	}

	c.add("service = ?", "payment-service")
	c.cursorBefore("01JBQ8K3M7VXFZ2N9WQYRT4HCD")

	const want = " WHERE service = ? AND id < ?"
	if got := c.where(); got != want {
		t.Fatalf("where() = %q, want %q", got, want)
	}
	if len(c.args) != 2 {
		t.Fatalf("args = %v, want one per placeholder", c.args)
	}
}

func TestConditionsIgnoresAnEmptyCursor(t *testing.T) {
	var c conditions
	c.cursorBefore("")
	if c.where() != "" || len(c.args) != 0 {
		t.Fatalf("an absent cursor added a clause: %q %v", c.where(), c.args)
	}
}

func TestVerifyDSN(t *testing.T) {
	tests := []struct {
		name    string
		dsn     string
		wantErr string
	}{
		{
			name: "complete",
			dsn:  "user:pw@tcp(127.0.0.1:3306)/incident_diag?parseTime=true&loc=UTC",
		},
		{
			name:    "parseTime missing",
			dsn:     "user:pw@tcp(127.0.0.1:3306)/incident_diag?loc=UTC",
			wantErr: "parseTime=true is missing",
		},
		{
			name:    "parseTime false",
			dsn:     "user:pw@tcp(127.0.0.1:3306)/incident_diag?parseTime=false&loc=UTC",
			wantErr: "parseTime must be true",
		},
		{
			name:    "loc missing",
			dsn:     "user:pw@tcp(127.0.0.1:3306)/incident_diag?parseTime=true",
			wantErr: "loc=UTC is missing",
		},
		{
			name:    "loc is not UTC",
			dsn:     "user:pw@tcp(127.0.0.1:3306)/incident_diag?parseTime=true&loc=Local",
			wantErr: "loc must be UTC",
		},
		{
			name:    "no parameters at all",
			dsn:     "user:pw@tcp(127.0.0.1:3306)/incident_diag",
			wantErr: "parseTime=true is missing",
		},
		{
			name:    "not a dsn",
			dsn:     "postgres://localhost/incident_diag",
			wantErr: "not a valid data source name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyDSN(tt.dsn)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyDSN returned %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("verifyDSN returned nil, want an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("verifyDSN error = %q, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestVerifyDSNReportsEveryProblemAtOnce(t *testing.T) {
	err := verifyDSN("user:pw@tcp(127.0.0.1:3306)/incident_diag?parseTime=false&loc=Local")
	if err == nil {
		t.Fatal("verifyDSN returned nil")
	}
	for _, want := range []string{"parseTime must be true", "loc must be UTC"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q too", err, want)
		}
	}
}

func TestRedactDSNKeepsThePasswordOutOfErrors(t *testing.T) {
	const dsn = "user:hunter2@tcp(127.0.0.1:3306)/incident_diag?parseTime=true&loc=UTC"
	err := redactDSN(fmt.Errorf("dial %s: refused", dsn), dsn)
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error still carries the password: %q", err)
	}
}

func TestIsDuplicateKeyMatchesOnlyTheNamedConstraint(t *testing.T) {
	activeRun := &mysql.MySQLError{
		Number:  errDuplicateEntry,
		Message: "Duplicate entry '01JBQ8K3M7VXFZ2N9WQYRT4HCD' for key 'agent_runs.uniq_active_run'",
	}
	stepNumber := &mysql.MySQLError{
		Number:  errDuplicateEntry,
		Message: "Duplicate entry '01JBQ8K3M7VXFZ2N9WQYRT4HCD-3' for key 'agent_steps.uniq_step_number'",
	}

	if !isDuplicateKey(fmt.Errorf("insert: %w", activeRun), keyActiveRun) {
		t.Error("the active-run violation was not recognised through wrapping")
	}
	if isDuplicateKey(stepNumber, keyActiveRun) {
		t.Error("a different unique key was treated as an active-run conflict")
	}
	if isDuplicateKey(errors.New("some other failure"), keyActiveRun) {
		t.Error("a non-MySQL error was treated as a duplicate key")
	}
}

func TestDbErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want httpx.Kind
	}{
		{"bad connection", driver.ErrBadConn, httpx.KindUnavailable},
		{"invalid connection", mysql.ErrInvalidConn, httpx.KindUnavailable},
		{"dial failure", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, httpx.KindUnavailable},
		{"too many connections", &mysql.MySQLError{Number: errTooManyConns}, httpx.KindUnavailable},
		{"server shutting down", &mysql.MySQLError{Number: errServerShutdown}, httpx.KindUnavailable},
		// A duplicate key is never a conflict by default. Only the callers that
		// know which constraint is meaningful translate one.
		{"unhandled duplicate key", &mysql.MySQLError{Number: errDuplicateEntry, Message: "for key 'agent_steps.uniq_step_number'"}, httpx.KindInternal},
		{"syntax error", &mysql.MySQLError{Number: 1064}, httpx.KindInternal},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := dbError(fmt.Errorf("wrapped: %w", tt.err), "some operation")
			if k := httpx.KindOf(got); k != tt.want {
				t.Fatalf("kind = %s, want %s", k, tt.want)
			}
			if !errors.Is(got, tt.err) {
				t.Error("the cause is no longer reachable through errors.Is")
			}
		})
	}
}

func TestDbErrorKeepsTheOperationOutOfTheClientMessage(t *testing.T) {
	err := dbError(errors.New("Table 'incident_diag.incidents' doesn't exist"), "select incident")
	if msg := httpx.Message(err); msg != "internal server error" {
		t.Fatalf("client message = %q, want the generic one", msg)
	}
	if !strings.Contains(err.Error(), "select incident") {
		t.Fatalf("the operation is missing from the logged error: %q", err)
	}
}

func TestDbErrorPassesNilThrough(t *testing.T) {
	if err := dbError(nil, "select incident"); err != nil {
		t.Fatalf("dbError(nil) = %v, want nil", err)
	}
}

func TestOpenRejectsADSNThatWouldBreakTimestamps(t *testing.T) {
	// Open must fail on the DSN itself, before it tries to connect: there is
	// no server at this address, and the test must not depend on that.
	_, err := Open(context.Background(), databaseConfig("user:pw@tcp(127.0.0.1:1)/incident_diag"))
	if err == nil {
		t.Fatal("Open accepted a DSN without parseTime or loc")
	}
	if !strings.Contains(err.Error(), "parseTime=true&loc=UTC") {
		t.Fatalf("error = %q, want it to name the missing parameters", err)
	}
}

func TestNullHelpers(t *testing.T) {
	if nullString("") != nil {
		t.Error("an empty string should become NULL")
	}
	if nullString("boom") != "boom" {
		t.Error("a non-empty string should be stored as itself")
	}
	if nullJSON(nil) != nil {
		t.Error("absent JSON should become NULL")
	}
	if got := nullJSON([]byte(`{"a":1}`)); got == nil {
		t.Error("present JSON should not become NULL")
	}
}

// databaseConfig is the minimum config the tests in this file need.
func databaseConfig(dsn string) config.Database {
	return config.Database{DSN: dsn, MaxOpenConns: 1, MaxIdleConns: 1}
}

func TestNormalizeBringsTheLimitIntoRange(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"zero value", 0, DefaultPageLimit},
		{"negative", -5, DefaultPageLimit},
		{"in range", 7, 7},
		{"over the maximum", MaxPageLimit + 1, MaxPageLimit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (PageParams{Limit: c.in}).normalize().Limit; got != c.want {
				t.Errorf("normalize(%d).Limit = %d, want %d", c.in, got, c.want)
			}
		})
	}
	if got := (PageParams{Limit: 3, Cursor: "abc"}).normalize().Cursor; got != "abc" {
		t.Errorf("normalize dropped the cursor: %q", got)
	}
}

// A page request the API cannot produce, but an in-process caller can: the
// agent worker, a maintenance job, a test. It used to index items[-1] and take
// the process down.
func TestPaginateSurvivesAnUnnormalizedLimit(t *testing.T) {
	got := paginate([]string{"B", "A"}, PageParams{}, func(s string) string { return s })
	if len(got.Items) != 0 || got.NextCursor != "" {
		t.Errorf("paginate(limit 0) = %+v, want an empty page", got)
	}
}
