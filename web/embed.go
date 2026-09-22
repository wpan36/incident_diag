// Package web carries the front end as an embedded file.
//
// It is embedded rather than read from disk so that `cmd/api` is
// self-contained, following migrations/embed.go: the binary serves the same
// page from a source checkout or a compose service, with no volume to mount
// and no working directory to get right.
//
// Serving it from the API also puts the page on the API's own origin, so there
// is no CORS configuration anywhere in this project.
//
// The package lives beside the file because go:embed cannot reach outside its
// own directory.
package web

import (
	_ "embed"
)

// Page is the whole front end: markup, styles and script in one file, with no
// build step (ADR 0007).
//
//go:embed index.html
var Page []byte
