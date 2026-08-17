// Package blob stores the archive's files.
//
// Files are the archive and the database is an index. The
// filesystem implementation is the one that keeps that promise — extracted
// articles and their images live in a dated, human-navigable tree that a
// browser can open with this service stopped and Postgres gone.
//
// Raw fetched pages, the article tree, index.html, meta.json, and the localized
// images all live here. The interface is deliberately small so that adding
// another backing store does not mean changing call sites.
//
// It exists because keeping the raw fetch is what makes every future extractor
// improvement reach the whole archive: `tome reextract` regenerates every body
// *without re-fetching*, which is only possible if the original is still here.
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
