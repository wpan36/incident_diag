package config

import (
	"fmt"
	"time"
)

// Database is the configuration needed to reach MySQL.
//
// It is loaded separately from Config rather than being folded into it. Load is
// shared by every binary in this project, so a required MYSQL_DSN there would
// stop ops-mcp and the Incident Lab services from starting over a database none
// of them touches. Each binary declares what it actually needs instead.
type Database struct {
	// DSN is a go-sql-driver/mysql data source name. It must carry
	// parseTime=true and loc=UTC; the store enforces that at startup, because
	// that is where the consequence of getting it wrong shows up.
	DSN string

	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// LoadDatabase reads the database configuration from the environment.
//
// Like Load, it reports every problem it finds at once. A binary that calls
// both gets the problems from both, so an operator still sees one complete
// list.
func LoadDatabase() (Database, error) {
	var e env

	db := Database{
		DSN:             e.requiredString("MYSQL_DSN"),
		MaxOpenConns:    e.optionalInt("DB_MAX_OPEN_CONNS", 25),
		MaxIdleConns:    e.optionalInt("DB_MAX_IDLE_CONNS", 25),
		ConnMaxLifetime: e.optionalDuration("DB_CONN_MAX_LIFETIME", 5*time.Minute),
	}

	if db.MaxOpenConns < 1 {
		e.fail("DB_MAX_OPEN_CONNS must be at least 1, got %d", db.MaxOpenConns)
	}
	if db.MaxIdleConns < 0 {
		e.fail("DB_MAX_IDLE_CONNS must not be negative, got %d", db.MaxIdleConns)
	}

	if err := e.err(); err != nil {
		return Database{}, err
	}
	return db, nil
}

// String renders the configuration for startup logging with the DSN omitted.
// A DSN carries the database password, so it must never be logged, and %+v on
// this struct at a call site would do exactly that.
func (d Database) String() string {
	return fmt.Sprintf("dsn=<redacted> max_open_conns=%d max_idle_conns=%d conn_max_lifetime=%s",
		d.MaxOpenConns, d.MaxIdleConns, d.ConnMaxLifetime)
}
