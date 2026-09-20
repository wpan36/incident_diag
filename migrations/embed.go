// Package migrations carries the SQL schema migrations as embedded files.
//
// They are embedded rather than read from disk so that a binary is
// self-contained: `cmd/migrate` applies the same schema whether it runs from a
// source checkout, a scratch container or a compose service, with no volume to
// mount and no path to get wrong.
//
// The package lives beside the .sql files because go:embed cannot reach outside
// its own directory.
package migrations

import "embed"

// FS holds the numbered migration files, in golang-migrate's
// <version>_<name>.<up|down>.sql naming.
//
//go:embed *.sql
var FS embed.FS
