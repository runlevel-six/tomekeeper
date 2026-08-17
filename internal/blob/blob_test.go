package blob_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runlevel-six/tomekeeper/internal/blob"
)

func newStore(t *testing.T) *blob.Filesystem {
	t.Helper()

	s, err := blob.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newStore(t)
	const path = "articles/2026/08/example-a1b2c3/raw.html.gz"
	const content = "the stored page"

	if err := s.Put(t.Context(), path, strings.NewReader(content)); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	r, err := s.Get(t.Context(), path)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	defer func() { _ = r.Close() }()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(got) != content {
		t.Errorf("read %q, want %q", got, content)
	}
}

func TestPutCreatesParentDirectories(t *testing.T) {
	s := newStore(t)

	if err := s.Put(t.Context(), "a/b/c/d/e.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "a", "b", "c", "d", "e.txt")); err != nil {
		t.Errorf("the file is not where it should be: %v", err)
	}
}

// A retried job must overwrite cleanly rather than appending to a partial file.
func TestPutOverwrites(t *testing.T) {
	s := newStore(t)
	const path = "x.txt"

	if err := s.Put(t.Context(), path, strings.NewReader("first version, longer")); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	if err := s.Put(t.Context(), path, strings.NewReader("second")); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	r, err := s.Get(t.Context(), path)
	if err != nil {
		t.Fatalf("Get() = %v", err)
	}
	defer func() { _ = r.Close() }()

	got, _ := io.ReadAll(r)
	if string(got) != "second" {
		t.Errorf("read %q, want the second version with no remnant of the first", got)
	}
}

// The write is atomic, so nothing ever observes a partial file — and no
// temporary files are left behind to accumulate in the archive.
func TestPutLeavesNoTemporaryFiles(t *testing.T) {
	s := newStore(t)

	if err := s.Put(t.Context(), "dir/file.txt", strings.NewReader("content")); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	entries, err := os.ReadDir(filepath.Join(s.Root(), "dir"))
	if err != nil {
		t.Fatalf("reading directory: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only the destination file", names)
	}
}

func TestGetMissing(t *testing.T) {
	s := newStore(t)

	if _, err := s.Get(t.Context(), "nothing/here.txt"); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get() = %v, want ErrNotFound", err)
	}
}

func TestExists(t *testing.T) {
	s := newStore(t)

	ok, err := s.Exists(t.Context(), "x.txt")
	if err != nil {
		t.Fatalf("Exists() = %v", err)
	}
	if ok {
		t.Error("Exists() = true before the blob was written")
	}

	if err := s.Put(t.Context(), "x.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	if ok, err = s.Exists(t.Context(), "x.txt"); err != nil || !ok {
		t.Errorf("Exists() = %v, %v; want true, nil", ok, err)
	}
}

// Deleting something already gone is not an error: the caller wanted it gone.
func TestDelete(t *testing.T) {
	s := newStore(t)

	if err := s.Delete(t.Context(), "never-existed.txt"); err != nil {
		t.Errorf("Delete() on a missing blob = %v, want nil", err)
	}

	if err := s.Put(t.Context(), "x.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put() = %v", err)
	}
	if err := s.Delete(t.Context(), "x.txt"); err != nil {
		t.Fatalf("Delete() = %v", err)
	}

	if ok, _ := s.Exists(t.Context(), "x.txt"); ok {
		t.Error("the blob still exists after Delete")
	}
}

// Blob paths are built from article URLs and content hashes — data that comes
// from the open internet. A traversal would let a remote site choose where
// this service writes files, so this is a security boundary.
func TestPathTraversalIsRejected(t *testing.T) {
	s := newStore(t)

	hostile := []string{
		"../escape.txt",
		"../../etc/passwd",
		"articles/../../escape.txt",
		"a/b/../../../escape.txt",
		"/absolute/path.txt",
		"/etc/passwd",
		"..",
		".",
		"",
		"with\x00null.txt",
	}

	for _, path := range hostile {
		t.Run(path, func(t *testing.T) {
			if err := s.Put(t.Context(), path, strings.NewReader("x")); err == nil {
				t.Errorf("Put(%q) = nil, want the path rejected", path)
			}
			if _, err := s.Get(t.Context(), path); err == nil {
				t.Errorf("Get(%q) = nil, want the path rejected", path)
			}
			if _, err := s.Exists(t.Context(), path); err == nil {
				t.Errorf("Exists(%q) = nil, want the path rejected", path)
			}
			if err := s.Delete(t.Context(), path); err == nil {
				t.Errorf("Delete(%q) = nil, want the path rejected", path)
			}
		})
	}

	// Nothing escaped: the parent of the root is untouched.
	parent := filepath.Dir(s.Root())
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Fatal("a traversal wrote outside the store root")
	}
}

// Interior ".." that stays inside the root is fine — only escaping is refused.
func TestInteriorDotDotIsAllowed(t *testing.T) {
	s := newStore(t)

	if err := s.Put(t.Context(), "a/b/../c.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("Put() = %v, want a path that stays inside the root to be accepted", err)
	}
	if _, err := os.Stat(filepath.Join(s.Root(), "a", "c.txt")); err != nil {
		t.Errorf("the file is not at the cleaned path: %v", err)
	}
}

func TestRelativeRootIsRejected(t *testing.T) {
	if _, err := blob.NewFilesystem("relative/path"); err == nil {
		t.Error("NewFilesystem() accepted a relative root, which would move the archive between deployments")
	}
}

func TestNewFilesystemCreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")

	s, err := blob.NewFilesystem(root)
	if err != nil {
		t.Fatalf("NewFilesystem() = %v", err)
	}
	if _, err := os.Stat(s.Root()); err != nil {
		t.Errorf("the root was not created: %v", err)
	}
}
