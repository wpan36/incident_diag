package migrations

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	mysqldriver "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	mysqlgo "github.com/go-sql-driver/mysql"
)

// Result describes what a migration run did. From is 0 when the database was
// empty, and To is 0 after a full down migration.
type Result struct {
	From uint
	To   uint
}

// Changed reports whether the run applied anything.
func (r Result) Changed() bool { return r.From != r.To }

func (r Result) String() string { return fmt.Sprintf("%d -> %d", r.From, r.To) }

// Up applies every migration that has not been applied yet.
//
// golang-migrate's MySQL driver takes an advisory lock for the duration, so two
// processes starting at once is safe: one waits for the other rather than
// applying the same DDL twice.
func Up(dsn string, logger *slog.Logger) (Result, error) {
	return run(dsn, logger, "applying migrations", func(m *migrate.Migrate) error { return m.Up() })
}

// Down reverses every applied migration, which drops every table.
//
// It exists so the down migrations are actually exercised rather than written
// and never run; a down migration nobody has executed is a guess. Nothing calls
// it automatically.
func Down(dsn string, logger *slog.Logger) (Result, error) {
	return run(dsn, logger, "reversing migrations", func(m *migrate.Migrate) error { return m.Down() })
}

// Version reports the applied schema version, and whether the database is
// dirty — left half-migrated by a run that failed partway, which every
// subsequent run will refuse until a human resolves it.
//
// A database with no migrations applied yet reports version 0.
func Version(dsn string) (version uint, dirty bool, err error) {
	_, err = run(dsn, nil, "reading the schema version", func(m *migrate.Migrate) error {
		v, d, err := m.Version()
		// ErrNilVersion is an empty database, which is version 0 rather than a
		// failure to read.
		if errors.Is(err, migrate.ErrNilVersion) {
			return nil
		}
		version, dirty = v, d
		return err
	})
	return version, dirty, err
}

func run(dsn string, logger *slog.Logger, what string, apply func(*migrate.Migrate) error) (Result, error) {
	src, err := iofs.New(FS, ".")
	if err != nil {
		return Result{}, fmt.Errorf("reading embedded migrations: %w", err)
	}

	dsn, err = multiStatementDSN(dsn)
	if err != nil {
		return Result{}, err
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return Result{}, fmt.Errorf("opening mysql: %w", err)
	}

	drv, err := mysqldriver.WithInstance(db, &mysqldriver.Config{})
	if err != nil {
		db.Close()
		return Result{}, fmt.Errorf("preparing the migration driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "mysql", drv)
	if err != nil {
		db.Close()
		return Result{}, fmt.Errorf("preparing the migrator: %w", err)
	}
	// Closing the migrator closes the *sql.DB it was given, which is why this
	// function owns the handle rather than accepting one from a caller that
	// would then find it closed underneath it.
	defer m.Close()

	if logger != nil {
		m.Log = migrateLogger{logger}
	}

	var res Result
	res.From, _ = currentVersion(m)

	// ErrNoChange is the ordinary "already up to date" answer, not a failure.
	if err := apply(m); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		res.To, _ = currentVersion(m)
		return res, fmt.Errorf("%s: %w", what, err)
	}
	res.To, _ = currentVersion(m)
	return res, nil
}

// multiStatementDSN turns on multiStatements for the migrator's own
// connection.
//
// Each migration file creates several tables, and MySQL rejects more than one
// statement per Exec unless the connection asks for it. The parameter is added
// here rather than required in MYSQL_DSN because it belongs to this connection
// only: the application's pool has no business executing several statements at
// once, and the usual objection to multiStatements — that it widens what an
// injected string can do — does not apply to files embedded in the binary.
func multiStatementDSN(dsn string) (string, error) {
	cfg, err := mysqlgo.ParseDSN(dsn)
	if err != nil {
		return "", fmt.Errorf("MYSQL_DSN is not a valid data source name: %w", err)
	}
	cfg.MultiStatements = true
	return cfg.FormatDSN(), nil
}

// currentVersion returns the applied schema version, reporting 0 for a database
// with no migrations applied yet.
//
// A dirty database — one where a migration failed partway — is reported as an
// error by golang-migrate, and the subsequent Up will fail with the same
// complaint, so this helper does not need to distinguish the two.
func currentVersion(m *migrate.Migrate) (uint, bool) {
	v, dirty, err := m.Version()
	if err != nil {
		return 0, false
	}
	return v, dirty
}

// migrateLogger adapts slog to the logger golang-migrate expects, so applying a
// migration appears in the same structured stream as everything else.
type migrateLogger struct{ l *slog.Logger }

func (m migrateLogger) Printf(format string, v ...any) {
	// golang-migrate writes lines terminated for a console; a structured
	// record should not carry the newline into its message field.
	m.l.Info(strings.TrimRight(fmt.Sprintf(format, v...), "\n"))
}

// Verbose false keeps golang-migrate's per-statement chatter out of the log; the
// version transition is what an operator needs to see.
func (m migrateLogger) Verbose() bool { return false }
