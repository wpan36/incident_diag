// Command migrate applies the embedded schema migrations to MySQL.
//
// It is a separate binary rather than something the API does at startup. The
// API is not the only process that needs the schema: the two workers would
// otherwise race a schema that may not exist yet. In Compose this becomes a
// service the others wait on with
// `depends_on: { condition: service_completed_successfully }`.
//
//	migrate          apply every pending migration (the default)
//	migrate up       the same, written out
//	migrate down     reverse every applied migration, dropping every table
//	migrate version  print the current schema version and exit
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/wpan36/incident_diag/internal/config"
	"github.com/wpan36/incident_diag/internal/log"
	"github.com/wpan36/incident_diag/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	command := "up"
	switch len(os.Args) {
	case 1:
	case 2:
		command = os.Args[1]
	default:
		return errors.New("usage: migrate [up|down|version]")
	}

	// Both loaders are called before either error is returned, so an operator
	// with two variables wrong is told about both at once.
	cfg, cfgErr := config.Load()
	db, dbErr := config.LoadDatabase()
	if err := errors.Join(cfgErr, dbErr); err != nil {
		return err
	}

	logger := log.New(os.Stderr, cfg.LogLevel)

	switch command {
	case "up":
		res, err := migrations.Up(db.DSN, logger)
		if err != nil {
			return err
		}
		if res.Changed() {
			logger.Info("migrations applied", "from", res.From, "to", res.To)
		} else {
			logger.Info("schema is already up to date", "version", res.To)
		}
	case "down":
		// Dropping every table is not something to do by accident, so it is
		// never the default and never inferred from an empty argument list.
		res, err := migrations.Down(db.DSN, logger)
		if err != nil {
			return err
		}
		logger.Info("migrations reversed", "from", res.From, "to", res.To)
	case "version":
		v, dirty, err := migrations.Version(db.DSN)
		if err != nil {
			return err
		}
		logger.Info("schema version", "version", v, "dirty", dirty)
	default:
		return fmt.Errorf("unknown command %q: usage: migrate [up|down|version]", command)
	}
	return nil
}
