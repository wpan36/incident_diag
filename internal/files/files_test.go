package files

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStorage(t *testing.T, maxBytes int64) *Storage {
	t.Helper()
	s, err := New(t.TempDir(), maxBytes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestSaveWritesHashesAndCountsInOnePass(t *testing.T) {
	s := newStorage(t, 1<<20)
	const content = "# payment-service runbook\n\nCheck the connection pool first.\n"

	saved, err := s.Save("01JBQ8M0YB4C3D2E1F0G9H8J7K", "runbook.md", strings.NewReader(content))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	if want := filepath.Join("01JBQ8M0YB4C3D2E1F0G9H8J7K", "runbook.md"); saved.Path != want {
		t.Errorf("Path = %q, want %q", saved.Path, want)
	}
	if filepath.IsAbs(saved.Path) {
		t.Error("Path is absolute; the database stores a path relative to the root")
	}
	if saved.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", saved.Size, len(content))
	}
	sum := sha256.Sum256([]byte(content))
	if saved.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("SHA256 = %s, want the hash of the content", saved.SHA256)
	}

	onDisk, err := os.ReadFile(filepath.Join(s.Root(), saved.Path))
	if err != nil {
		t.Fatalf("reading the stored file: %v", err)
	}
	if string(onDisk) != content {
		t.Error("the stored bytes differ from what was sent")
	}
}

// TestSaveKeepsTwoUploadsOfTheSameNameApart is why the path carries the
// document id: two uploads of runbook.md are a normal thing for a user to do.
func TestSaveKeepsTwoUploadsOfTheSameNameApart(t *testing.T) {
	s := newStorage(t, 1<<20)

	first, err := s.Save("01JBQ8M0YB4C3D2E1F0G9H8J7K", "runbook.md", strings.NewReader("one"))
	if err != nil {
		t.Fatalf("first Save: %v", err)
	}
	second, err := s.Save("01JBQ8M0YB4C3D2E1F0G9H8J7L", "runbook.md", strings.NewReader("two"))
	if err != nil {
		t.Fatalf("second Save: %v", err)
	}
	if first.Path == second.Path {
		t.Fatalf("both uploads landed at %q", first.Path)
	}

	content, err := os.ReadFile(filepath.Join(s.Root(), first.Path))
	if err != nil || string(content) != "one" {
		t.Fatalf("the first file was overwritten: %q, err %v", content, err)
	}
}

func TestSaveRejectsANameThatCarriesAPath(t *testing.T) {
	s := newStorage(t, 1<<20)

	names := []string{
		"../escape.md",
		"../../etc/passwd",
		"nested/runbook.md",
		`windows\runbook.md`,
		"/absolute.md",
		"..",
		".",
		"",
		"null\x00.md",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Save("01JBQ8M0YB4C3D2E1F0G9H8J7K", name, strings.NewReader("x")); err == nil {
				t.Fatalf("Save accepted %q", name)
			}
		})
	}

	// Nothing escaped, and nothing was left behind by the attempts.
	entries, err := os.ReadDir(s.Root())
	if err != nil {
		t.Fatalf("reading the root: %v", err)
	}
	for _, e := range entries {
		t.Errorf("the root contains %q after only rejected uploads", e.Name())
	}
}

func TestSaveRejectsAMaliciousDocumentID(t *testing.T) {
	s := newStorage(t, 1<<20)
	if _, err := s.Save("../elsewhere", "runbook.md", strings.NewReader("x")); err == nil {
		t.Fatal("Save accepted a document id containing a path")
	}
}

func TestSaveRefusesAFileOverTheLimitAndLeavesNothingBehind(t *testing.T) {
	const max = 64
	s := newStorage(t, max)

	_, err := s.Save("01JBQ8M0YB4C3D2E1F0G9H8J7K", "runbook.md", strings.NewReader(strings.Repeat("a", max+1)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}

	if _, err := os.Stat(filepath.Join(s.Root(), "01JBQ8M0YB4C3D2E1F0G9H8J7K")); !os.IsNotExist(err) {
		t.Fatalf("the partial file was left on disk: %v", err)
	}
}

func TestSaveAcceptsAFileExactlyAtTheLimit(t *testing.T) {
	const max = 64
	s := newStorage(t, max)

	saved, err := s.Save("01JBQ8M0YB4C3D2E1F0G9H8J7K", "runbook.md", strings.NewReader(strings.Repeat("a", max)))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if saved.Size != max {
		t.Fatalf("Size = %d, want %d", saved.Size, max)
	}
}

func TestSaveRefusesToOverwrite(t *testing.T) {
	s := newStorage(t, 1<<20)
	const docID = "01JBQ8M0YB4C3D2E1F0G9H8J7K"

	if _, err := s.Save(docID, "runbook.md", strings.NewReader("one")); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if _, err := s.Save(docID, "runbook.md", strings.NewReader("two")); err == nil {
		t.Fatal("Save overwrote an existing file")
	}
}

func TestRemoveDeletesEverythingForADocument(t *testing.T) {
	s := newStorage(t, 1<<20)
	const docID = "01JBQ8M0YB4C3D2E1F0G9H8J7K"

	if _, err := s.Save(docID, "runbook.md", strings.NewReader("one")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Remove(docID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), docID)); !os.IsNotExist(err) {
		t.Fatalf("the document directory survived Remove: %v", err)
	}

	// Removing what is already gone is not an error: it runs on failure paths
	// where a second error would hide the first.
	if err := s.Remove(docID); err != nil {
		t.Fatalf("removing an absent document: %v", err)
	}
}

func TestRemoveRejectsAMaliciousDocumentID(t *testing.T) {
	s := newStorage(t, 1<<20)
	if err := s.Remove("../.."); err == nil {
		t.Fatal("Remove accepted a document id containing a path")
	}
}

func TestNewMakesTheRootAbsoluteAndCreatesIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), "documents", "nested")
	s, err := New(root, 1<<20)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !filepath.IsAbs(s.Root()) {
		t.Errorf("Root = %q, want an absolute path", s.Root())
	}
	if info, err := os.Stat(s.Root()); err != nil || !info.IsDir() {
		t.Fatalf("the root was not created: %v", err)
	}
}

func TestNewRejectsAnEmptyRoot(t *testing.T) {
	if _, err := New("", 1<<20); err == nil {
		t.Fatal("New accepted an empty root")
	}
}
