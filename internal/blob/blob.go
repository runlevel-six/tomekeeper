// Package blob stores the archive's files.
//
// Principle 2.4: files are the archive and the database is an index. The
// filesystem implementation is the one that keeps that promise — extracted
// articles and their images live in a dated, human-navigable tree that a
// browser can open with this service stopped and Postgres gone.
//
// At M2 only raw fetched pages are stored here. The article tree, index.html,
// meta.json, and assets arrive with M3; the interface is settled now so that
// adding them does not mean changing call sites.
//
// M2 needs this at all because of principle 2.2 and M2's own acceptance
// criterion: `tome reextract` must regenerate every body *without re-fetching*.
// That is only possible if the raw fetch was kept.
package blob

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned by Get and by Delete when a path does not exist.
var ErrNotFound = errors.New("blob not found")

// Store is the archive's file storage.
//
// Paths are slash-separated and relative to the store's root. An
// implementation must reject anything that escapes the root.
type Store interface {
	Put(ctx context.Context, path string, r io.Reader) error
	Get(ctx context.Context, path string) (io.ReadCloser, error)
	Exists(ctx context.Context, path string) (bool, error)
	Delete(ctx context.Context, path string) error
}
