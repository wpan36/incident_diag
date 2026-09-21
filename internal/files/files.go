// Package files stores uploaded document files on disk.
//
// It is separate from internal/store because the two answer to different
// failures: MySQL holds the metadata and the ingestion state, and this holds
// the bytes. Keeping them apart is also what lets the write ordering be
// explicit — the file is on disk before the row exists, so a crash leaves an
// orphaned file rather than a row pointing at nothing.
package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrTooLarge is returned when the incoming file exceeds the configured limit.
// The upload handler answers it with 413.
var ErrTooLarge = errors.New("file exceeds the maximum upload size")

// Storage writes document files under a single root directory.
type Storage struct {
	root     string
	maxBytes int64
}

// Saved describes a file that reached disk.
type Saved struct {
	// Path is relative to the root, which is what goes in the database: the
	// rows then stay valid whatever the host layout is, in Compose or on a
	// developer's machine.
	Path   string
	Size   int64
	SHA256 string
}

// New prepares the storage root, creating it if it does not exist.
//
// The root is made absolute immediately so that nothing later depends on the
// process's working directory, which a service has no business caring about.
func New(root string, maxBytes int64) (*Storage, error) {
	if root == "" {
		return nil, errors.New("files: the storage root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("files: resolving the storage root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("files: creating the storage root: %w", err)
	}
	return &Storage{root: abs, maxBytes: maxBytes}, nil
}

// Root returns the absolute storage root.
func (s *Storage) Root() string { return s.root }

// MaxBytes returns the configured upload limit.
func (s *Storage) MaxBytes() int64 { return s.maxBytes }

// Save streams r into <documentID>/<filename> under the root, hashing and
// counting it in the same pass.
//
// The ULID directory makes collisions impossible without consulting the
// database, which matters because two uploads of runbook.md are a normal thing
// for a user to do, and keeping the original name keeps the shared volume
// readable when something goes wrong.
//
// Exceeding maxBytes returns ErrTooLarge and leaves nothing behind.
func (s *Storage) Save(documentID, filename string, r io.Reader) (Saved, error) {
	if err := checkName(documentID); err != nil {
		return Saved{}, fmt.Errorf("files: document id: %w", err)
	}
	// The handler has already validated this. Checking again is not
	// redundant: this is the function that turns a name into a path, so it is
	// the one place where being wrong lets an upload choose where it lands.
	if err := checkName(filename); err != nil {
		return Saved{}, fmt.Errorf("files: filename: %w", err)
	}

	dir := filepath.Join(s.root, documentID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Saved{}, fmt.Errorf("files: creating the document directory: %w", err)
	}

	full := filepath.Join(dir, filename)
	// O_EXCL rather than truncate: a document id is generated per upload, so an
	// existing file here means something is wrong and overwriting it would hide
	// that.
	f, err := os.OpenFile(full, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return Saved{}, fmt.Errorf("files: creating the file: %w", err)
	}

	sum := sha256.New()
	// One byte past the limit, so exceeding it is detected rather than
	// silently truncating the document to exactly the cap.
	n, copyErr := io.Copy(io.MultiWriter(f, sum), io.LimitReader(r, s.maxBytes+1))
	closeErr := f.Close()

	switch {
	case copyErr != nil:
		s.Remove(documentID)
		return Saved{}, fmt.Errorf("files: writing the file: %w", copyErr)
	case closeErr != nil:
		s.Remove(documentID)
		return Saved{}, fmt.Errorf("files: closing the file: %w", closeErr)
	case n > s.maxBytes:
		s.Remove(documentID)
		return Saved{}, ErrTooLarge
	}

	return Saved{
		Path:   filepath.Join(documentID, filename),
		Size:   n,
		SHA256: hex.EncodeToString(sum.Sum(nil)),
	}, nil
}

// Open opens a stored document file for reading. The caller closes it.
//
// It takes the two components separately rather than the stored path, and puts
// each through checkName exactly as Save does, because this is the second
// function in this package that turns stored input into a filesystem path. The
// write path already defends against ".." and the Windows backslash spelling;
// a read path that does not is a way back in. The caller has the documents row,
// which carries both the id and the filename, so storage_path never has to be
// parsed as a path by anything.
func (s *Storage) Open(documentID, filename string) (*os.File, error) {
	if err := checkName(documentID); err != nil {
		return nil, fmt.Errorf("files: document id: %w", err)
	}
	if err := checkName(filename); err != nil {
		return nil, fmt.Errorf("files: filename: %w", err)
	}

	f, err := os.Open(filepath.Join(s.root, documentID, filename))
	if err != nil {
		// Named the way the documents row names it, not the way the filesystem
		// does. This error becomes a document's failure_reason, which a user
		// reads back through GET /api/documents, and the storage root is the
		// server's business rather than something to hand out with it.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return nil, fmt.Errorf("files: opening %s: %w",
				filepath.Join(documentID, filename), pathErr.Err)
		}
		return nil, fmt.Errorf("files: opening the file: %w", err)
	}
	return f, nil
}

// Remove deletes everything stored for a document.
//
// It is best effort and called on the failure paths of an upload, where the
// request is already being answered with an error and a leftover file is a
// smaller problem than a second error hiding the first.
func (s *Storage) Remove(documentID string) error {
	if err := checkName(documentID); err != nil {
		return fmt.Errorf("files: document id: %w", err)
	}
	return os.RemoveAll(filepath.Join(s.root, documentID))
}

// checkName rejects anything that could make a path component mean something
// other than itself.
//
// filepath.Base is the test: a name it does not leave intact is a name that
// carries directory structure, and an upload is not allowed to influence where
// it lands. The explicit separator check covers the Windows spelling, which
// Base does not strip on Linux but which some clients still send.
func checkName(name string) error {
	switch {
	case name == "":
		return errors.New("must not be empty")
	case name == "." || name == "..":
		return errors.New("must not be a directory reference")
	case strings.ContainsAny(name, `/\`):
		return errors.New("must not contain a path separator")
	case strings.ContainsRune(name, 0):
		return errors.New("must not contain a null byte")
	case filepath.Base(name) != name:
		return errors.New("must be a bare file name")
	}
	return nil
}
