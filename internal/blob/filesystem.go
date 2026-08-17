package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Permissions for everything this store creates.
//
// Group-readable but not world-readable. The archive is a complete record of
// what one person reads, which is worth keeping off a shared machine's
// world-readable paths; group read is retained deliberately so the §10 backup
// job can replicate the tree without running as the worker's own user.
//
// Principle 2.4 is unaffected either way: opening index.html with the service
// stopped is something the owner does, and the owner can always read its own
// archive.
const (
	dirMode  fs.FileMode = 0o750
	fileMode fs.FileMode = 0o640
)

// Filesystem stores blobs as files under a root directory.
type Filesystem struct {
	root string
}

// NewFilesystem returns a store rooted at dir, creating it if necessary.
//
// The root must be absolute. A relative root would resolve against whatever
// directory the process started in, which means the archive could quietly end
// up in a different place on the next deployment — and an archive nobody can
// find is an archive that has been lost.
func NewFilesystem(root string) (*Filesystem, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("blob root %q must be an absolute path", root)
	}
	if err := os.MkdirAll(root, dirMode); err != nil {
		return nil, fmt.Errorf("creating blob root %s: %w", root, err)
	}
	return &Filesystem{root: root}, nil
}

// Root returns the directory this store writes to.
func (f *Filesystem) Root() string { return f.root }

// Put writes a blob, creating parent directories as needed.
//
// The write goes to a temporary file in the destination directory and is then
// renamed into place. On a POSIX filesystem that rename is atomic, so a reader
// — or a crash — never sees a half-written article. It also means a retried
// job overwrites cleanly instead of appending to a partial file.
func (f *Filesystem) Put(_ context.Context, path string, r io.Reader) error {
	full, err := f.resolve(path)
	if err != nil {
		return err
	}

	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	// Best-effort cleanup if anything below fails; a successful rename makes
	// this a no-op.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := io.Copy(tmp, r); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// Durability before visibility: fsync, then rename. Without the sync, a
	// power loss can leave the rename applied and the contents empty.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, fileMode); err != nil {
		return fmt.Errorf("setting permissions on %s: %w", path, err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return fmt.Errorf("renaming into %s: %w", path, err)
	}
	return nil
}

// Get opens a blob for reading. The caller must close it.
func (f *Filesystem) Get(_ context.Context, path string) (io.ReadCloser, error) {
	full, err := f.resolve(path)
	if err != nil {
		return nil, err
	}

	// resolve has already confirmed the path stays inside the root, which is
	// what G304 is asking about; see its doc comment for why that check is a
	// security boundary rather than tidiness.
	file, err := os.Open(full) //nolint:gosec // path validated by resolve
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s: %w", path, ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return file, nil
}

// Exists reports whether a blob is present.
func (f *Filesystem) Exists(_ context.Context, path string) (bool, error) {
	full, err := f.resolve(path)
	if err != nil {
		return false, err
	}

	switch _, err := os.Stat(full); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("checking %s: %w", path, err)
	}
}

// Delete removes a blob. Deleting something that is not there is not an error:
// the caller wanted it gone, and it is.
func (f *Filesystem) Delete(_ context.Context, path string) error {
	full, err := f.resolve(path)
	if err != nil {
		return err
	}

	if err := os.Remove(full); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("deleting %s: %w", path, err)
	}
	return nil
}

// resolve turns a store-relative path into an absolute one, refusing anything
// that would escape the root.
//
// The paths handled here are built from article URLs and content hashes — data
// that comes from the open internet. A path containing "../" would let a
// remote site choose where this service writes files, so the check is a
// security boundary rather than tidiness.
func (f *Filesystem) resolve(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("blob path must not be empty")
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("blob path %q contains a null byte", path)
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("blob path %q must be relative to the store root", path)
	}

	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("blob path %q escapes the store root", path)
	}

	full := filepath.Join(f.root, clean)

	// Belt and braces: after cleaning and joining, the result must still be
	// inside the root. This catches anything the string checks above missed,
	// including platform-specific separator handling.
	rel, err := filepath.Rel(f.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("blob path %q escapes the store root", path)
	}
	return full, nil
}

var _ Store = (*Filesystem)(nil)
