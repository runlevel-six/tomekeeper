package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/runlevel-six/tomekeeper/internal/backup"
	"github.com/runlevel-six/tomekeeper/internal/db"
)

// restoreCmd loads an archive written by `tome backup`.
//
// **Stop the writers first.** Nothing here can arrange that, which is why this is a
// command and not a route in the web interface: a restore truncates the tables and
// rewrites the tree, and a worker still fetching into them would be writing into a
// database that is being replaced underneath it.
//
// It migrates before it loads, so a restore onto a newer build works without a
// separate step — and it refuses an archive taken at a schema this build cannot reach,
// which is the direction that cannot be repaired.
func restoreCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)

	force := fs.Bool("force", false, "replace an archive this database already holds")
	quiet := fs.Bool("quiet", false, "report nothing but failures")
	fs.Usage = func() { restoreUsage(stderr) }

	path, ok := parsePositional(fs, args, "path to an archive", stderr)
	if !ok {
		restoreUsage(stderr)
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

	// Migrations first, so restoring onto a newer build is one command rather than
	// two with an ordering to remember. A dump carries whatever schema it was taken
	// at, and `serve` refuses to start against a mismatch — loudly, but after the
	// fact.
	if err := db.Migrate(ctx, pool, log); err != nil {
		log.Error("migration failed", "error", err)
		return exitFailure
	}

	opts := backup.RestoreOptions{BlobRoot: cfg.BlobRoot, Force: *force}
	if !*quiet {
		opts.Progress = func(stage string, done, total int) {
			fmt.Fprintf(stderr, "\r%s: %d/%d", stage, done, total)
			if done == total {
				fmt.Fprintln(stderr)
			}
		}
	}

	result, err := backup.Restore(ctx, pool, path, opts)
	if err != nil {
		log.Error("the restore failed", "error", err)
		return exitFailure
	}

	m := result.Manifest
	fmt.Fprintf(stdout, "restored %s and %s (%s) from an archive taken %s at schema %d\n",
		plural(int(result.Rows), "row"), plural(result.Files, "file"),
		humanBytes(result.Bytes), m.TakenAt.Format("2006-01-02 15:04 MST"), m.SchemaVersion)

	if n := len(m.Missing); n > 0 {
		// Said again here, because this is the moment it becomes real: those articles
		// are in the database and their bytes are not on disk.
		fmt.Fprintf(stdout, "%s referenced by the database was not in the archive, so those "+
			"articles have no stored page or image; `tome refetch` is the way back for the ones "+
			"whose sites are still up\n", plural(n, "file"))
	}

	// The queue is deliberately absent from an archive, so say what happens next
	// rather than leaving a worker's first minute looking like a malfunction.
	fmt.Fprintln(stdout, "the job queue was not restored: the schedulers rebuild it within a minute "+
		"of `tome worker` starting")
	fmt.Fprintln(stdout, "open an article with images before calling this done — that is the check "+
		"the database alone cannot give you")
	return exitOK
}

func restoreUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: tome restore PATH [--force] [--quiet]

Loads an archive written by `+"`tome backup`"+`: migrates the schema, replaces every
table, and unpacks the file tree.

**Stop the writers first.** This truncates the tables it loads, so a worker or a
server still writing into them will fight the restore.

It refuses a database that already holds articles unless --force is given, and
refuses an archive taken at a schema this build cannot reach.

Flags:
  --force   replace an archive this database already holds
  --quiet   report nothing but failures
`)
}
