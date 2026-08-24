package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/backup"
	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/version"
)

// backupCmd writes one file holding both halves of the archive.
//
// It exists in the binary rather than in a runbook because an archive can be running
// under Kubernetes, under Compose, or from a systemd unit, and the image is distroless
// — so every shell recipe necessarily ran outside the application, in whatever the
// platform provided. See internal/backup for the format and the ordering it depends on.
func backupCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)

	to := fs.String("to", "", "write to this path instead of standard output")
	verify := fs.String("verify", "", "check an existing archive and exit; needs no database")
	force := fs.Bool("force", false, "overwrite the file named by --to")
	quiet := fs.Bool("quiet", false, "report nothing but failures")
	fs.Usage = func() { backupUsage(stderr) }

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		backupUsage(stderr)
		return exitUsage
	}

	// Verification is deliberately its own path: no configuration, no database, no
	// archive tree. The question "is this backup any good" has to be answerable on
	// whatever machine the file was copied to, months later, by somebody holding only
	// the file and this binary.
	if *verify != "" {
		return verifyArchive(*verify, stdout, stderr)
	}

	if *to == "" && *force {
		fmt.Fprintln(stderr, "tome backup: --force only means anything with --to")
		return exitUsage
	}

	cfg, log, code := loadConfigAndLogger(stderr)
	if code != exitOK {
		return code
	}

	ctx, stop := signalContext()
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("cannot reach the database", "error", err)
		return exitFailure
	}
	defer pool.Close()

	// Progress on stderr, so `tome backup > archive.tar` stays a clean pipe. Silent
	// when nobody asked and when writing to standard output anyway.
	opts := backup.Options{BlobRoot: cfg.BlobRoot, Version: version.Short()}
	if !*quiet {
		opts.Progress = func(stage string, done, total int) {
			fmt.Fprintf(stderr, "\r%s: %d/%d", stage, done, total)
			if done == total {
				fmt.Fprintln(stderr)
			}
		}
	}

	if *to == "" {
		result, err := backup.Write(ctx, pool, stdout, opts)
		if err != nil {
			log.Error("the backup failed", "error", err)
			return exitFailure
		}
		reportBackup(stderr, result, *quiet)
		return exitOK
	}

	// A directory means "you pick the name", and the name rotates by day of the week —
	// the convention the database dump CronJob has always used, now available without a
	// shell. That matters more than it sounds: the image is distroless, so a scheduled
	// job cannot expand $(date +%u), and without this the only options were one file
	// overwritten forever or a shell in the backup path.
	target := *to
	rotating := false
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		target = filepath.Join(target, fmt.Sprintf("tome-%d.tar", int(time.Now().UTC().Weekday())+1))
		rotating = true
	}

	// Written to a temporary name and moved into place, so an interrupted backup
	// never leaves a truncated file that looks like one. The same reasoning the
	// database dump CronJob has always used, and the reason it has never produced a
	// half file that somebody trusted.
	partial := target + ".partial"
	if _, err := os.Stat(target); err == nil && !*force && !rotating {
		fmt.Fprintf(stderr, "tome backup: %s exists; pass --force to replace it\n", target)
		return exitUsage
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		fmt.Fprintf(stderr, "tome backup: %v\n", err)
		return exitFailure
	}
	//nolint:gosec // 0640 is the archive's documented mode; see reference/storage-layout.md
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o640)
	if err != nil {
		fmt.Fprintf(stderr, "tome backup: %v\n", err)
		return exitFailure
	}

	result, writeErr := backup.Write(ctx, pool, f, opts)
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(partial)
		log.Error("the backup failed", "error", writeErr)
		return exitFailure
	}
	if closeErr != nil {
		_ = os.Remove(partial)
		fmt.Fprintf(stderr, "tome backup: %v\n", closeErr)
		return exitFailure
	}
	if err := os.Rename(partial, target); err != nil {
		fmt.Fprintf(stderr, "tome backup: %v\n", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "wrote %s\n", target)
	reportBackup(stdout, result, *quiet)
	fmt.Fprintf(stdout, "check it with: tome backup --verify %s\n", target)
	return exitOK
}

func reportBackup(w io.Writer, result *backup.Result, quiet bool) {
	if quiet {
		return
	}
	m := result.Manifest
	var rows int64
	for _, t := range m.Tables {
		rows += t.Rows
	}
	fmt.Fprintf(w, "%s, schema %d: %s across %s, and %s\n",
		humanBytes(result.Bytes), m.SchemaVersion,
		plural(int(rows), "row"), plural(len(m.Tables), "table"),
		plural(len(m.Files), "file"))

	// Said whenever it is not zero, because it is the one thing a backup cannot fix
	// afterwards: those rows will restore without their bytes.
	if n := len(m.Missing); n > 0 {
		fmt.Fprintf(w, "the database references %s that the tree no longer has — most likely a "+
			"prune or an expiry ran during the backup\n", plural(n, "file"))
		for _, p := range m.Missing {
			if n <= 10 {
				fmt.Fprintf(w, "  %s\n", p)
			}
		}
	}
}

// verifyArchive checks one archive and says what it found.
func verifyArchive(path string, stdout, stderr io.Writer) int {
	f, err := os.Open(path) //nolint:gosec // the path is the operator's own argument
	if err != nil {
		fmt.Fprintf(stderr, "tome backup: %v\n", err)
		return exitFailure
	}
	defer func() { _ = f.Close() }()

	report, err := backup.Verify(f)
	if err != nil {
		fmt.Fprintf(stderr, "tome backup: %v\n", err)
		return exitFailure
	}

	m := report.Manifest
	fmt.Fprintf(stdout, "%s, written by tome %s at %s, schema %d\n",
		humanBytes(report.Bytes), m.Tome, m.TakenAt.Format("2006-01-02 15:04 MST"), m.SchemaVersion)
	fmt.Fprintf(stdout, "%s\n", report.Summary())

	if len(m.Missing) > 0 {
		fmt.Fprintf(stdout, "the database referenced %s that were already gone when this was "+
			"taken, so those rows will restore without their bytes\n", plural(len(m.Missing), "file"))
	}

	if report.OK() {
		fmt.Fprintln(stdout, "this archive is whole")
		return exitOK
	}

	// The failure the M7 drill found: a copy that ended early and exited 0. Named
	// precisely, because "restore failed" six months from now is not a diagnosis.
	if n := len(report.Absent); n > 0 {
		fmt.Fprintf(stderr, "%s the manifest names is missing from the archive:\n", plural(n, "entry"))
		for i, p := range report.Absent {
			if i == 10 {
				fmt.Fprintf(stderr, "  … and %d more\n", n-i)
				break
			}
			fmt.Fprintf(stderr, "  %s\n", p)
		}
	}
	if n := len(report.Corrupt); n > 0 {
		fmt.Fprintf(stderr, "%s does not match the hash recorded for it:\n", plural(n, "entry"))
		for i, p := range report.Corrupt {
			if i == 10 {
				fmt.Fprintf(stderr, "  … and %d more\n", n-i)
				break
			}
			fmt.Fprintf(stderr, "  %s\n", p)
		}
	}
	fmt.Fprintln(stderr, "this is not a usable backup: take another one")
	return exitFailure
}

func backupUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: tome backup [--to PATH] [--force] [--quiet]
       tome backup --verify PATH

Writes one archive holding both halves of this archive: every table, the whole
file tree, and a manifest recording a hash for every file the database records
one for. With no --to it streams to standard output.

--verify reads an archive and checks it against its own manifest. It needs no
database and no configuration, so a backup can be checked wherever it ended up.

Restore with "tome restore PATH", which needs the writers stopped.

Flags:
  --to PATH     write here instead of to standard output, via a .partial file.
                A directory means "you choose the name": tome-N.tar, rotating by
                day of the week, which is how a scheduled job rotates without a
                shell to run date(1) in.
  --force       replace the file named by --to
  --verify PATH check an archive and exit
  --quiet       report nothing but failures
`)
}
