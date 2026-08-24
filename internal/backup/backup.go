// Package backup writes and reads the archive's own backup format: one file
// carrying both halves of an archive, and enough information to prove it is whole.
//
// **Why this is in the binary rather than in a runbook.** An archive can be running
// under Kubernetes, under Docker Compose, or from a systemd unit on somebody's own
// machine, and until now the automated half existed only for the first of those while
// everyone else got two shell commands in a how-to. Worse, the image is distroless —
// no shell, no tar — so every recipe necessarily ran *outside* the application, in
// whatever the platform happened to provide. The binary is the one place that is the
// same everywhere, and the only place that knows which bytes matter.
//
// **The two halves have to travel together.** The database is an index over the
// files; the files are the archive. A restore of one without the other looks entirely
// healthy from every command that reports on it and renders blank frames to a reader.
// So a backup here is a single stream containing the tables, the tree, and a manifest
// that says what should be in it.
//
// **It can prove itself, which a copy cannot.** Every irreplaceable file already has
// a hash recorded in the database — `assets.sha256` for an image, `articles.raw_blob_sha`
// for a fetched page — so the manifest is not a promise this code makes, it is what the
// archive already claims about itself. The 2026-08-20 restore drill is the argument:
// a documented `kubectl run --rm -i` streaming a tar delivered 73.9 MB of 307 MB, 37
// files of 8,946, and exit code 0. Verification is the difference between a copy and
// a backup, and it took a hand-built checksum manifest to notice.
//
// **What it is not.** It is not an export: `tome export` is one reader's articles in a
// portable, importable shape. This is the household's bytes, restorable onto a new
// machine, and it is only ever readable by the software that wrote it.
package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FormatVersion is the archive layout's own version.
//
// Bumped when the layout changes in a way an older `tome restore` could not read.
// Recorded in the manifest so a restore refuses a future archive by saying so rather
// than by failing partway through one.
const FormatVersion = 1

// Names inside the archive. Fixed strings rather than derived, because a restore has
// to find them in a stream it is reading once.
const (
	readmeName   = "README"
	manifestName = "manifest.json"
	dbPrefix     = "db/"
	blobPrefix   = "blobs/"
)

// Kind says what a file in the tree is, which decides whether it can be verified.
const (
	// KindAsset and KindRaw are the irreplaceable bytes, and the database records a
	// hash for each: an image and a fetched page.
	KindAsset = "asset"
	KindRaw   = "raw"

	// KindDerived is everything extraction writes out of them — index.html and
	// meta.json. Carried, because nothing regenerates them on demand today, but
	// unverifiable: no hash for them is recorded anywhere, and inventing one here
	// would only prove this code hashed what it just read.
	KindDerived = "derived"
)

// Manifest is what the archive claims to contain.
type Manifest struct {
	FormatVersion int    `json:"format_version"`
	Tome          string `json:"tome_version"`

	// SchemaVersion is the goose migration the tables were dumped at. A restore
	// migrates to at least this before loading, and refuses an archive from a newer
	// build than itself.
	SchemaVersion int64 `json:"schema_version"`

	TakenAt time.Time `json:"taken_at"`

	Tables []TableEntry `json:"tables"`
	Files  []FileEntry  `json:"files"`

	// Missing names files the dumped rows reference that were not in the tree when
	// it was walked. Ordinarily empty. Recorded rather than treated as a failure,
	// because the honest cause is a prune or an expiry that ran during the backup —
	// and a backup that refuses to finish over a file the archive itself has already
	// released would be a worse outcome than one that says which rows will come back
	// without their bytes.
	Missing []string `json:"missing,omitempty"`
}

// TableEntry is one dumped table.
type TableEntry struct {
	Name    string   `json:"name"`
	Entry   string   `json:"entry"`
	Columns []string `json:"columns"`
	Rows    int64    `json:"rows"`
	SHA256  string   `json:"sha256"`
}

// FileEntry is one file from the archive tree.
type FileEntry struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
	Kind  string `json:"kind"`

	// SHA256 is what the database says this file's contents hash to, for the kinds
	// where it says anything. Empty for a derived file.
	SHA256 string `json:"sha256,omitempty"`
}

// Verifiable reports whether this entry can be checked against a recorded hash.
func (f FileEntry) Verifiable() bool { return f.SHA256 != "" }

// Options configure a backup.
type Options struct {
	// BlobRoot is the archive tree to copy.
	BlobRoot string

	// Version is the build writing the archive, recorded in the manifest.
	Version string

	// Progress, when set, is called as work completes so a long backup can say
	// something before it finishes.
	Progress func(stage string, done, total int)
}

// Result reports what a backup wrote.
type Result struct {
	Manifest Manifest
	Bytes    int64
}

// Write streams a complete backup to w.
//
// The order is the load-bearing part, and it is not the obvious one. The database
// snapshot is taken **first**, in a repeatable-read transaction, and the tree is
// walked after it. That way every row the archive contains refers to a file that
// existed when the snapshot was taken, so anything written during the walk is extra
// bytes rather than a row without its file. The reverse order — files first, then a
// dump — reads more naturally and produces exactly that inconsistency for every
// article fetched while the backup runs.
//
// The manifest is written last, because it carries what was actually found. A reader
// verifying the archive streams it once, hashing as it goes, and compares at the end.
func Write(ctx context.Context, pool *pgxpool.Pool, w io.Writer, opts Options) (*Result, error) {
	if opts.BlobRoot == "" {
		return nil, fmt.Errorf("no archive tree configured, so there is nothing to back up")
	}

	counted := &countingWriter{w: w}
	tw := tar.NewWriter(counted)

	// A repeatable-read snapshot, so the tables agree with each other. Without it a
	// long dump can carry an article_content row whose article arrived after the
	// articles table was read, and the restore fails on a foreign key at the far end
	// of a long operation.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("opening a snapshot to back up from: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := checkTablesAreKnown(ctx, tx); err != nil {
		return nil, err
	}

	manifest := Manifest{
		FormatVersion: FormatVersion,
		Tome:          opts.Version,
		TakenAt:       time.Now().UTC(),
	}
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version WHERE is_applied`).
		Scan(&manifest.SchemaVersion); err != nil {
		return nil, fmt.Errorf("reading the schema version: %w", err)
	}

	if err := writeEntry(tw, readmeName, []byte(readme)); err != nil {
		return nil, err
	}

	// The tables, from the snapshot.
	for i, table := range dumpOrder {
		entry, err := dumpTable(ctx, tx, tw, table)
		if err != nil {
			return nil, err
		}
		manifest.Tables = append(manifest.Tables, *entry)
		if opts.Progress != nil {
			opts.Progress("tables", i+1, len(dumpOrder))
		}
	}

	// What the snapshot says the tree should contain. Read before the walk so that
	// the two can be compared afterwards: this is the set whose absence matters.
	expected, err := expectedFiles(ctx, tx)
	if err != nil {
		return nil, err
	}

	files, err := writeTree(ctx, tw, opts, expected)
	if err != nil {
		return nil, err
	}
	manifest.Files = files

	written := make(map[string]bool, len(files))
	for _, f := range files {
		written[f.Path] = true
	}
	for p := range expected {
		if !written[p] {
			manifest.Missing = append(manifest.Missing, p)
		}
	}
	sort.Strings(manifest.Missing)

	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding the manifest: %w", err)
	}
	if err := writeEntry(tw, manifestName, body); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("closing the archive: %w", err)
	}

	return &Result{Manifest: manifest, Bytes: counted.n}, nil
}

// dumpTable copies one table into the archive as gzipped COPY text.
//
// Gzipped per entry rather than compressing the whole archive: the tables are text and
// compress by a large factor, while the images are already compressed and would only
// cost CPU. Hashing the stored bytes rather than the plaintext keeps verification a
// single pass with no decompression.
func dumpTable(ctx context.Context, tx pgx.Tx, tw *tar.Writer, table string) (*TableEntry, error) {
	cols, err := tableColumns(ctx, tx, table)
	if err != nil {
		return nil, err
	}

	var rows int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+quote(table)).Scan(&rows); err != nil {
		return nil, fmt.Errorf("counting %s: %w", table, err)
	}

	// Buffered, because a tar entry needs its size in its header before its bytes.
	// A table's text is bounded by the archive's own content, which is already
	// entirely in the database this is reading from.
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)

	copySQL := fmt.Sprintf("COPY (SELECT %s FROM %s) TO STDOUT",
		quoteAll(cols), quote(table))
	if _, err := tx.Conn().PgConn().CopyTo(ctx, gz, copySQL); err != nil {
		return nil, fmt.Errorf("dumping %s: %w", table, err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("compressing %s: %w", table, err)
	}

	stored := buf.Bytes()
	sum := sha256.Sum256(stored)

	name := dbPrefix + table + ".copy.gz"
	if err := writeEntry(tw, name, stored); err != nil {
		return nil, err
	}
	return &TableEntry{
		Name:    table,
		Entry:   name,
		Columns: cols,
		Rows:    rows,
		SHA256:  hex.EncodeToString(sum[:]),
	}, nil
}

// expectedFiles is every tree path the snapshot records a hash for.
func expectedFiles(ctx context.Context, tx pgx.Tx) (map[string]FileEntry, error) {
	out := make(map[string]FileEntry)

	rows, err := tx.Query(ctx, `
		SELECT fs_path, sha256 FROM assets WHERE COALESCE(fs_path, '') <> ''`)
	if err != nil {
		return nil, fmt.Errorf("listing the archived images: %w", err)
	}
	for rows.Next() {
		var p, sha string
		if err := rows.Scan(&p, &sha); err != nil {
			rows.Close()
			return nil, err
		}
		out[path.Clean(p)] = FileEntry{Path: path.Clean(p), Kind: KindAsset, SHA256: sha}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	raw, err := tx.Query(ctx, `
		SELECT raw_blob_path, raw_blob_sha FROM articles
		WHERE COALESCE(raw_blob_path, '') <> '' AND COALESCE(raw_blob_sha, '') <> ''`)
	if err != nil {
		return nil, fmt.Errorf("listing the stored pages: %w", err)
	}
	defer raw.Close()
	for raw.Next() {
		var p, sha string
		if err := raw.Scan(&p, &sha); err != nil {
			return nil, err
		}
		out[path.Clean(p)] = FileEntry{Path: path.Clean(p), Kind: KindRaw, SHA256: sha}
	}
	return out, raw.Err()
}

// writeTree copies the archive tree into the archive, in a stable order.
func writeTree(ctx context.Context, tw *tar.Writer, opts Options, expected map[string]FileEntry) ([]FileEntry, error) {
	root := filepath.Clean(opts.BlobRoot)

	// Checked before the walk, because filepath.WalkDir reports a missing root as an
	// lstat error on a path the reader did not type, and the actual question is
	// whether TOME_BLOB_ROOT points where the archive lives.
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("the archive tree at %s cannot be read, so this backup would "+
			"contain no files: check %sBLOB_ROOT", root, "TOME_")
	}

	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// A lost+found on a mounted volume is the filesystem's, not the
			// archive's, and it is usually unreadable to a non-root process.
			if d.Name() == "lost+found" {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		paths = append(paths, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking the archive tree at %s: %w", root, err)
	}
	sort.Strings(paths)

	out := make([]FileEntry, 0, len(paths))
	for i, rel := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		full := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Stat(full)
		if err != nil {
			// A file that vanished between the walk and here is a prune or an
			// expiry doing its job. It is reported through Missing if the snapshot
			// referenced it, and ignored otherwise.
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("reading %s: %w", rel, err)
		}

		entry := expected[rel]
		if entry.Path == "" {
			entry = FileEntry{Path: rel, Kind: KindDerived}
		}
		entry.Bytes = info.Size()

		if err := copyFileEntry(tw, blobPrefix+rel, full, info); err != nil {
			return nil, err
		}
		out = append(out, entry)

		if opts.Progress != nil && (i+1)%500 == 0 {
			opts.Progress("files", i+1, len(paths))
		}
	}
	if opts.Progress != nil {
		opts.Progress("files", len(out), len(paths))
	}
	return out, nil
}

func copyFileEntry(tw *tar.Writer, name, full string, info fs.FileInfo) error {
	f, err := os.Open(full) //nolint:gosec // a path under the archive root this was given
	if err != nil {
		return fmt.Errorf("opening %s: %w", full, err)
	}
	defer func() { _ = f.Close() }()

	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    0o640,
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}); err != nil {
		return fmt.Errorf("writing the header for %s: %w", name, err)
	}
	// Copied with the size already committed to the header, so a file being appended
	// to underneath us would produce a short write rather than a corrupt archive.
	n, err := io.Copy(tw, f)
	if err != nil {
		return fmt.Errorf("copying %s: %w", name, err)
	}
	if n != info.Size() {
		return fmt.Errorf("%s changed size while being copied: %d bytes of %d", name, n, info.Size())
	}
	return nil
}

func writeEntry(tw *tar.Writer, name string, body []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    0o640,
		Size:    int64(len(body)),
		ModTime: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("writing the header for %s: %w", name, err)
	}
	if _, err := tw.Write(body); err != nil {
		return fmt.Errorf("writing %s: %w", name, err)
	}
	return nil
}

// quote renders an identifier for interpolation into SQL.
//
// Every value that reaches this comes from dumpOrder or from information_schema, so
// nothing here is reader input — but a COPY statement cannot take an identifier as a
// parameter, so the quoting is written rather than assumed.
func quote(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

func quoteAll(idents []string) string {
	out := make([]string, len(idents))
	for i, id := range idents {
		out[i] = quote(id)
	}
	return strings.Join(out, ", ")
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

const readme = `This is a Tomekeeper backup: one archive holding both halves of an
archive, and a manifest saying what should be in it.

  db/       one gzipped PostgreSQL COPY file per table
  blobs/    the archive tree: fetched pages, images, and the pages written from them
  manifest.json  what this archive claims to contain, including a hash for every
                 file the database records one for

To check it without a database, anywhere the binary runs:

    tome backup --verify <this file>

To restore it onto an empty archive, with the writers stopped:

    tome restore <this file>

Restore migrates the schema first, loads the tables, and unpacks the tree. It
refuses a database that already holds an archive unless asked twice.

The job queue is deliberately not included: it is rebuilt from the archive within a
minute of the worker starting, which is better than resurrecting jobs about articles
that may since have been pruned.
`
