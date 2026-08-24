package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RestoreOptions configure a restore.
type RestoreOptions struct {
	// BlobRoot is where the archive tree is written.
	BlobRoot string

	// Force allows a restore into a database that already holds an archive. Without
	// it, a database with any articles in it is refused: restoring over a live
	// archive is the one mistake here that cannot be undone, and it is far more
	// likely to be a wrong argument than an intention.
	Force bool

	// Progress, when set, is called as work completes.
	Progress func(stage string, done, total int)
}

// RestoreResult reports what was loaded.
type RestoreResult struct {
	Manifest Manifest
	Rows     int64
	Files    int
	Bytes    int64
}

// Restore loads an archive into an empty database and archive tree.
//
// **Two passes over the file, which is why this takes a path rather than a reader.**
// The first reads the manifest — which lives at the end, because it records what was
// actually written — so that every refusal happens before anything has been changed:
// a format from a newer build, a schema this binary cannot reach, a database that
// already holds an archive. A restore that discovers its objection halfway through is
// worse than one that cannot start.
//
// Backup streams to stdout and restore does not read from stdin, and that asymmetry is
// deliberate rather than an omission. A backup is written once and forwarded; a
// restore is the operation you want to be able to think about before it begins.
//
// The writers must be stopped first. Nothing here can arrange that — it is why this
// stays a command and never becomes a route in the web interface.
func Restore(ctx context.Context, pool *pgxpool.Pool, path string, opts RestoreOptions) (*RestoreResult, error) {
	if opts.BlobRoot == "" {
		return nil, fmt.Errorf("no archive tree configured, so there is nowhere to restore the files to")
	}

	manifest, err := readManifest(path)
	if err != nil {
		return nil, err
	}
	if manifest.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("this archive is format version %d and this build understands %d; "+
			"restore it with the version of tome that wrote it, or newer",
			manifest.FormatVersion, FormatVersion)
	}

	var schema int64
	if err := pool.QueryRow(ctx,
		`SELECT COALESCE(max(version_id), 0) FROM goose_db_version WHERE is_applied`).Scan(&schema); err != nil {
		return nil, fmt.Errorf("reading this database's schema version: %w", err)
	}
	if schema < manifest.SchemaVersion {
		return nil, fmt.Errorf("this archive was taken at schema %d and this database is at %d; "+
			"run `tome migrate` first, or use a build new enough to reach it",
			manifest.SchemaVersion, schema)
	}

	// Refused rather than merged. The tables are truncated below, so a restore into a
	// live archive would destroy it — and the likeliest reason to be here with a
	// populated database is a mistyped argument.
	var articles int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM articles`).Scan(&articles); err != nil {
		return nil, fmt.Errorf("checking whether this database is empty: %w", err)
	}
	if articles > 0 && !opts.Force {
		return nil, fmt.Errorf("this database already holds %d articles; restoring would replace them.\n"+
			"Restore into an empty database, or pass --force if replacing this archive is the intention",
			articles)
	}

	result := &RestoreResult{Manifest: *manifest}

	if err := loadTables(ctx, pool, path, manifest, opts, result); err != nil {
		return nil, err
	}
	if err := unpackTree(ctx, path, opts, result); err != nil {
		return nil, err
	}
	return result, nil
}

// readManifest scans an archive for its manifest without keeping anything else.
func readManifest(path string) (*Manifest, error) {
	f, err := os.Open(path) //nolint:gosec // the path is the operator's own argument
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	tr := tar.NewReader(f)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, ErrNoManifest
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		if header.Name != manifestName {
			continue
		}
		var m Manifest
		if err := json.NewDecoder(tr).Decode(&m); err != nil {
			return nil, fmt.Errorf("parsing the manifest: %w", err)
		}
		return &m, nil
	}
}

// loadTables truncates and reloads every table the manifest carries, in one
// transaction.
//
// All of it or none of it: a restore that failed on the ninth table would leave a
// database holding part of one archive and part of another, which is a state nothing
// else in this system can describe.
func loadTables(ctx context.Context, pool *pgxpool.Pool, path string, manifest *Manifest,
	opts RestoreOptions, result *RestoreResult,
) error {
	byEntry := make(map[string]TableEntry, len(manifest.Tables))
	for _, t := range manifest.Tables {
		byEntry[t.Entry] = t
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("opening a transaction to restore into: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Truncated in reverse dependency order, and in one statement so the foreign keys
	// between them are never briefly violated.
	names := make([]string, 0, len(dumpOrder))
	for _, t := range dumpOrder {
		names = append(names, quote(t))
	}
	if _, err := tx.Exec(ctx, "TRUNCATE "+strings.Join(names, ", ")+" CASCADE"); err != nil {
		return fmt.Errorf("clearing the tables before loading: %w", err)
	}

	f, err := os.Open(path) //nolint:gosec // the path is the operator's own argument
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	// The archive stores the tables in dependency order, so streaming it in order is
	// also a valid load order — no seeking, and no second pass.
	tr := tar.NewReader(f)
	loaded := 0
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if header.Typeflag != tar.TypeReg || !strings.HasPrefix(header.Name, dbPrefix) {
			continue
		}
		entry, ok := byEntry[header.Name]
		if !ok {
			return fmt.Errorf("the archive holds %s, which its manifest does not mention", header.Name)
		}

		gz, err := gzip.NewReader(tr)
		if err != nil {
			return fmt.Errorf("decompressing %s: %w", header.Name, err)
		}
		copySQL := fmt.Sprintf("COPY %s (%s) FROM STDIN",
			quote(entry.Name), quoteAll(entry.Columns))
		tag, err := tx.Conn().PgConn().CopyFrom(ctx, gz, copySQL)
		if err != nil {
			return fmt.Errorf("loading %s: %w", entry.Name, err)
		}
		if err := gz.Close(); err != nil {
			return fmt.Errorf("finishing %s: %w", header.Name, err)
		}
		if tag.RowsAffected() != entry.Rows {
			return fmt.Errorf("%s restored %d rows and the manifest says %d",
				entry.Name, tag.RowsAffected(), entry.Rows)
		}
		result.Rows += tag.RowsAffected()

		loaded++
		if opts.Progress != nil {
			opts.Progress("tables", loaded, len(manifest.Tables))
		}
	}
	if loaded != len(manifest.Tables) {
		return fmt.Errorf("the archive carried %d of the %d tables its manifest names",
			loaded, len(manifest.Tables))
	}

	// Sequences, without which the next insert collides with a restored row: the same
	// repair EnsureSeedUser does for its explicit id=1, generalized.
	//
	// Which tables have one is asked rather than assumed. `assets` is keyed by content
	// hash and has no id column at all, and pg_get_serial_sequence *errors* on a column
	// that does not exist rather than returning null — so the existence check has to
	// come first, in the same query.
	seqRows, err := tx.Query(ctx, `
		SELECT c.table_name, pg_get_serial_sequence(c.table_name, 'id')
		FROM information_schema.columns c
		WHERE c.table_schema = 'public' AND c.column_name = 'id'
		  AND c.table_name = ANY($1)`, dumpOrder)
	if err != nil {
		return fmt.Errorf("looking for the id sequences: %w", err)
	}
	type sequence struct{ table, name string }
	var sequences []sequence
	for seqRows.Next() {
		var table string
		var name *string
		if err := seqRows.Scan(&table, &name); err != nil {
			seqRows.Close()
			return fmt.Errorf("scanning a sequence: %w", err)
		}
		if name != nil {
			sequences = append(sequences, sequence{table: table, name: *name})
		}
	}
	seqRows.Close()
	if err := seqRows.Err(); err != nil {
		return err
	}
	for _, seq := range sequences {
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`SELECT setval($1, GREATEST((SELECT COALESCE(max(id), 1) FROM %s), 1))`,
			quote(seq.table)), seq.name); err != nil {
			return fmt.Errorf("resetting the id sequence for %s: %w", seq.table, err)
		}
	}

	return tx.Commit(ctx)
}

// unpackTree writes the archive tree, refusing any path that would escape the root.
func unpackTree(ctx context.Context, path string, opts RestoreOptions, result *RestoreResult) error {
	root, err := filepath.Abs(opts.BlobRoot)
	if err != nil {
		return fmt.Errorf("resolving the archive root: %w", err)
	}
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", root, err)
	}

	f, err := os.Open(path) //nolint:gosec // the path is the operator's own argument
	if err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	tr := tar.NewReader(f)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if header.Typeflag != tar.TypeReg || !strings.HasPrefix(header.Name, blobPrefix) {
			continue
		}

		rel := strings.TrimPrefix(header.Name, blobPrefix)
		// The archive is written by this program, but an archive is a file somebody
		// can hand you: a path containing .. or an absolute path is how a tar becomes
		// a way to write anywhere the process can reach.
		dest := filepath.Join(root, filepath.FromSlash(rel))
		if !strings.HasPrefix(dest, root+string(os.PathSeparator)) {
			return fmt.Errorf("the archive contains %q, which is outside the archive root", rel)
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return fmt.Errorf("creating the directory for %s: %w", rel, err)
		}
		// 0640, matching what the archive writes: group read for a backup job, and
		// nothing for anybody else, since this tree is a complete record of what one
		// household reads.
		//nolint:gosec // 0640 is the archive's documented mode: group read for the
		// backup job, nothing for anybody else. See reference/storage-layout.md.
		out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
		if err != nil {
			return fmt.Errorf("creating %s: %w", rel, err)
		}
		n, err := io.Copy(out, io.LimitReader(tr, maxEntryBytes))
		if closeErr := out.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("writing %s: %w", rel, err)
		}

		result.Files++
		result.Bytes += n
		if opts.Progress != nil && result.Files%500 == 0 {
			opts.Progress("files", result.Files, len(result.Manifest.Files))
		}
	}
	if opts.Progress != nil {
		opts.Progress("files", result.Files, len(result.Manifest.Files))
	}
	return nil
}
