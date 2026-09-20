// Package store is the persistence layer over MySQL.
//
// It is hand-written database/sql rather than an ORM, for two reasons. The
// queries here are few and the interesting ones — the conditional updates that
// implement the state machines — are exactly the queries an ORM makes harder to
// read. And the idempotency this project relies on (ADR 0003) is a property of
// those UPDATE statements, so they are worth having in plain sight.
//
// Two rules hold throughout:
//
// State transitions are conditional updates. A transition method returns
// whether it changed a row; zero rows affected means the transition was not
// legal from the current state, which under at-least-once delivery normally
// means the message is a redelivery of work already done.
//
// Errors are returned already classified by internal/httpx, so a handler can
// map one to a status code without inspecting SQL internals. The coupling is to
// a classification package with no HTTP server dependency, which is what it was
// built for.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/httpx"
)

// MySQL server error numbers this package reacts to.
const (
	errDuplicateEntry   = 1062
	errTooManyConns     = 1040
	errServerShutdown   = 1053
	errCannotConnectNow = 1203
)

// Unique key names that carry meaning outside the database. A 1062 is only
// translated to a client-visible conflict when it names one of these.
const keyActiveRun = "uniq_active_run"

// Store is the handle on MySQL. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open connects to MySQL, verifies the DSN and the connection, and applies the
// pool settings.
//
// It pings before returning, so a wrong host or password fails at startup with
// a clear message instead of on the first request.
func Open(ctx context.Context, cfg config.Database) (*Store, error) {
	if err := verifyDSN(cfg.DSN); err != nil {
		return nil, err
	}

	db, err := sql.Open("mysql", cfg.DSN)
	if err != nil {
		// sql.Open only parses; a failure here is a malformed DSN, and its
		// text can contain the password, so it is not wrapped verbatim.
		return nil, fmt.Errorf("opening mysql: %w", redactDSN(err, cfg.DSN))
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connecting to mysql: %w", redactDSN(err, cfg.DSN))
	}
	return &Store{db: db}, nil
}

// New wraps an already-open handle. Tests that bring their own connection use
// it; production code goes through Open.
func New(db *sql.DB) *Store { return &Store{db: db} }

// DB exposes the underlying handle for the migration runner, which needs a
// *sql.DB rather than a query API.
func (s *Store) DB() *sql.DB { return s.db }

// Ping reports whether MySQL is reachable. The readiness probe calls it with a
// short timeout.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return httpx.UnavailableErr(err, "database is unavailable")
	}
	return nil
}

// Close releases the connection pool.
func (s *Store) Close() error { return s.db.Close() }

// verifyDSN rejects a DSN that would make timestamps wrong.
//
// Both parameters have to be there. Without parseTime, go-sql-driver hands a
// DATETIME(6) back as []byte and every scan into a time.Time fails. Without
// loc=UTC the driver interprets those values in the local zone, which quietly
// reintroduces exactly the machine-dependence that choosing DATETIME over
// TIMESTAMP was meant to remove.
//
// The check is on the text as well as on the parsed value, because the driver
// happens to default Loc to UTC today: a DSN that relies on that default is
// correct by accident, and this is not a thing to leave to accident.
func verifyDSN(dsn string) error {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return fmt.Errorf("MYSQL_DSN is not a valid data source name: %w", redactDSN(err, dsn))
	}
	params, err := dsnParams(dsn)
	if err != nil {
		return fmt.Errorf("MYSQL_DSN: %w", err)
	}

	var problems []string
	if !params.Has("parseTime") {
		problems = append(problems, "parseTime=true is missing")
	} else if !cfg.ParseTime {
		problems = append(problems, "parseTime must be true")
	}
	if !params.Has("loc") {
		problems = append(problems, "loc=UTC is missing")
	} else if cfg.Loc != time.UTC {
		problems = append(problems, fmt.Sprintf("loc must be UTC, got %s", cfg.Loc))
	}
	if len(problems) > 0 {
		return fmt.Errorf("MYSQL_DSN must carry parseTime=true&loc=UTC: %s", strings.Join(problems, "; "))
	}
	return nil
}

// dsnParams returns the query parameters of a go-sql-driver DSN, which has the
// form [user[:password]@][net[(addr)]]/dbname[?param=value&...].
//
// The scan starts from the last '/' the way the driver's own parser does: a
// password may contain '?' and an address may contain '/', so neither can be
// found reliably from the front.
func dsnParams(dsn string) (url.Values, error) {
	slash := strings.LastIndex(dsn, "/")
	if slash < 0 {
		return nil, errors.New("missing the '/' before the database name")
	}
	q := strings.Index(dsn[slash:], "?")
	if q < 0 {
		return url.Values{}, nil
	}
	params, err := url.ParseQuery(dsn[slash+q+1:])
	if err != nil {
		return nil, errors.New("its parameters are not valid query syntax")
	}
	return params, nil
}

// redactDSN removes the DSN from an error message. Driver errors quote the DSN
// they failed on, and a DSN carries the database password; this error is on its
// way to a log line.
func redactDSN(err error, dsn string) error {
	if dsn == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), dsn, "<dsn>")
	return errors.New(msg)
}

// dbError classifies an error from database/sql for the HTTP layer.
//
// op names the operation ("insert incident") and is kept for the logs only: it
// is attached to the wrapped cause, never to the client-facing message.
//
// Duplicate keys are deliberately not handled here. A 1062 is not
// conflict-shaped in general — violating the unique key on
// (run_id, step_number) is a bug in the agent loop, not something a client
// should see as a 409 — so the only callers that translate one are the ones
// that know which constraint would be meaningful, and everything else lands on
// the internal branch below and reaches the logs as a 500.
func dbError(err error, op string) error {
	if err == nil {
		return nil
	}
	if isUnavailable(err) {
		return httpx.UnavailableErr(fmt.Errorf("%s: %w", op, err), "database is unavailable")
	}
	return httpx.Internal(fmt.Errorf("%s: %w", op, err), "internal server error")
}

// isUnavailable reports whether err means the database could not be reached or
// refused to work, rather than that the statement was wrong.
//
// context.Canceled and context.DeadlineExceeded are left out on purpose:
// httpx.KindOf already classifies both, and a cancelled request is usually the
// client leaving rather than the database failing.
func isUnavailable(err error) bool {
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, mysql.ErrInvalidConn) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		switch me.Number {
		case errTooManyConns, errServerShutdown, errCannotConnectNow:
			return true
		}
	}
	return false
}

// isDuplicateKey reports whether err is a MySQL 1062 on the named unique key.
//
// Matching on the key name means depending on the text of a driver error, which
// is unpleasant but is the only identifier MySQL provides: 1062 carries the
// constraint name in its message and nowhere else. An integration test asserts
// both branches, so a driver upgrade that changes the wording fails loudly
// instead of turning conflicts into 500s.
func isDuplicateKey(err error, key string) bool {
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != errDuplicateEntry {
		return false
	}
	return strings.Contains(me.Message, key)
}

// scanner is the part of *sql.Row and *sql.Rows that the row-scanning helpers
// need, so one helper serves both the single-row and the list query.
type scanner interface {
	Scan(dest ...any) error
}

// now returns the current time as the database will store it.
//
// Truncating to microseconds matters: DATETIME(6) holds six fractional digits
// and MySQL rounds anything longer, so a time.Time with nanosecond precision
// would not compare equal to the value read back from the row it was written
// to.
func now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// summaryLimitBytes caps a persisted observation or tool result.
//
// A Prometheus range query can return megabytes, and an audit trail nobody can
// read is not an audit trail. The number is a guess and should be revisited
// once there is real tool output to look at.
const summaryLimitBytes = 8 << 10 // 8 KiB

// truncateSummary caps s at summaryLimitBytes bytes of UTF-8 and reports the
// original length together with whether it was cut.
//
// The three results are returned together so no caller can record one without
// the others — a stored summary whose byte count describes a different string
// is worse than no byte count at all.
//
// The cut falls on a rune boundary at or below the cap, never inside a
// multi-byte sequence. Slicing at a fixed byte offset would produce invalid
// UTF-8, which a utf8mb4 column will reject or silently mangle, and tool output
// contains non-ASCII text often enough for that to be a matter of when rather
// than whether.
func truncateSummary(s string) (summary string, length int, truncated bool) {
	if len(s) <= summaryLimitBytes {
		return s, len(s), false
	}
	cut := summaryLimitBytes
	// s[cut] is the first byte that will be dropped. While it is a
	// continuation byte the cut is inside a rune, so step back.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], len(s), true
}

// Page limits. The API validates the caller's limit against them; the store
// trusts what it is given.
const (
	DefaultPageLimit = 20
	MaxPageLimit     = 100
)

// PageParams is a validated cursor page request.
//
// Cursor is the id of the last item of the previous page, or "" for the first
// page. Because ULIDs sort by creation time the cursor needs no encoding, and
// the window does not shift under concurrent inserts the way an offset would.
type PageParams struct {
	Limit  int
	Cursor string
}

// Page is one page of results, newest first.
type Page[T any] struct {
	Items []T
	// NextCursor is "" on the last page.
	NextCursor string
}

// normalize brings a page request into range.
//
// The API rejects a limit outside the bounds rather than correcting it, so
// nothing arriving through a handler needs this. It is for the callers that do
// not come through one — the agent worker, a maintenance job, a test — where
// the zero value would otherwise ask for a page of no rows, which the cursor
// arithmetic below cannot express and used to answer with a panic. Every list
// method normalizes before it uses Limit, so the fetch and the pagination
// always agree on what it is.
func (p PageParams) normalize() PageParams {
	switch {
	case p.Limit < 1:
		p.Limit = DefaultPageLimit
	case p.Limit > MaxPageLimit:
		p.Limit = MaxPageLimit
	}
	return p
}

// fetchLimit is how many rows to ask for: one more than the caller wanted.
//
// The extra row is what makes the last page detectable. Inferring it from
// "fewer rows than limit" would be wrong whenever the final page is exactly
// full, handing the client a cursor that fetches nothing — a bug that only
// appears when the row count is a multiple of the page size, which is precisely
// when nobody is testing.
func (p PageParams) fetchLimit() int { return p.Limit + 1 }

// paginate turns the result of a fetchLimit query into a page, dropping the
// sentinel row and deriving the cursor from the last item actually returned.
func paginate[T any](rows []T, p PageParams, idOf func(T) string) Page[T] {
	if rows == nil {
		// The API renders an empty page as [], never null.
		rows = []T{}
	}
	if len(rows) <= p.Limit {
		return Page[T]{Items: rows}
	}
	items := rows[:p.Limit]
	if len(items) == 0 {
		// Unreachable once the caller has normalized, and cheaper than the
		// panic it replaces if one ever does not.
		return Page[T]{Items: items}
	}
	return Page[T]{Items: items, NextCursor: idOf(items[len(items)-1])}
}

// conditions accumulates WHERE clauses and their arguments for a list query.
//
// It exists so that every listing composes its filters and its cursor the same
// way. Concatenating fragments by hand is where "WHERE" and "AND" get confused
// once a filter becomes optional, and the result of getting the cursor
// comparison backwards is not an error but a silently wrong page.
type conditions struct {
	clauses []string
	args    []any
}

// add appends one clause. Anything variable in it must be a placeholder: no
// caller of this package formats a value into SQL.
func (c *conditions) add(clause string, args ...any) {
	c.clauses = append(c.clauses, clause)
	c.args = append(c.args, args...)
}

// cursorBefore restricts the listing to rows older than the cursor. Listings
// are newest first and ids sort by creation time, so "older" is "less than".
func (c *conditions) cursorBefore(cursor string) {
	if cursor != "" {
		c.add("id < ?", cursor)
	}
}

// where returns the WHERE clause, or "" when nothing was added.
func (c *conditions) where() string {
	if len(c.clauses) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(c.clauses, " AND ")
}

// changed reports whether a conditional update matched.
//
// go-sql-driver reports rows *changed* rather than rows matched, which is the
// stricter reading and the one wanted here: every transition below alters the
// status column, so an update that changes nothing really did fail its
// condition rather than rewrite a row with identical values.
func changed(res sql.Result, op string) (bool, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return false, dbError(err, op)
	}
	return n > 0, nil
}

// nullString turns an empty string into a SQL NULL.
//
// The nullable text columns here — a failure reason, a stop reason, an error —
// all mean "there isn't one" when empty, and storing "" instead of NULL would
// make a row that has no error indistinguishable from one whose error text was
// lost.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullJSON turns absent JSON into a SQL NULL. A JSON column rejects the empty
// string, so this is not merely a matter of taste.
func nullJSON(b json.RawMessage) any {
	if len(b) == 0 {
		return nil
	}
	return []byte(b)
}
