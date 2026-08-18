package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/exchange"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// exportArchive writes the reader's archive as a file the importer can read back.
//
// The half of the archive that lives in the database. The other half is the blob
// tree — the stored original pages and the localized images — and this file names
// those by path rather than carrying them, because base64 of a decade of pictures
// is not a document anybody can open. Both halves together are the archive; the
// command says so on the way out rather than leaving it to be discovered.
//
// Written to stdout by default, so it composes: piping into gzip, into a bucket, or
// into another machine's `tome import` are all the same command with no temporary
// file in between.
func exportArchive(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "write to this file instead of stdout")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: tome export [--out FILE]")
		fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fs.Usage()
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

	s := store.New(pool)

	userID, err := s.System().LookupUser(ctx, cfg.Username)
	if err != nil {
		fmt.Fprintf(stderr, "tome export: no user %q; run `tome migrate` first\n", cfg.Username)
		return exitFailure
	}

	// The summary goes to stderr whenever the export itself is going to stdout, so
	// that a pipe carries the archive and nothing else. A file destination gets the
	// summary on stdout, where somebody watching a terminal will see it.
	summary := stderr

	writer := stdout
	if *out != "" {
		// G304 wants a constant path; the operator names the destination, and that
		// is the whole flag.
		file, err := os.Create(*out) //nolint:gosec // the path is the operator's own argument
		if err != nil {
			fmt.Fprintf(stderr, "tome export: %v\n", err)
			return exitFailure
		}
		defer func() { _ = file.Close() }()

		writer = file
		summary = stdout
	}

	result, err := exchange.Export(ctx, s, userID, writer)
	if err != nil {
		log.Error("export failed", "error", err)
		// Said explicitly, because a half-written export that looks like a whole one
		// is the failure this whole command exists to prevent.
		fmt.Fprintf(stderr, "tome export: failed after %d articles; the output is incomplete\n",
			result.Articles)
		return exitFailure
	}

	if *out != "" {
		if file, ok := writer.(*os.File); ok {
			if err := file.Close(); err != nil {
				fmt.Fprintf(stderr, "tome export: writing %s: %v\n", *out, err)
				return exitFailure
			}
		}
	}

	printExportSummary(summary, *out, result)
	return exitOK
}

func printExportSummary(w io.Writer, path string, r exchange.ExportResult) {
	where := "stdout"
	if path != "" {
		where = path
	}

	fmt.Fprintf(w, "exported %s to %s: %d bodies, %d tags, %d highlights, %d images referenced\n",
		plural(r.Articles, "article"), where, r.Bodies, r.Tags, r.Highlights, r.Assets)

	if r.WithoutBody > 0 {
		fmt.Fprintf(w, "%s carry no body: a fetch that failed, or a body retention released. "+
			"The article, its metadata and your reading state are still exported.\n",
			plural(r.WithoutBody, "article"))
	}

	if r.Assets > 0 {
		// The sentence this command exists to say out loud. An operator who believes
		// this file is the whole archive finds out otherwise at the worst possible
		// moment.
		fmt.Fprintf(w, "Images and stored pages are referenced by path, not included. "+
			"Copy %s alongside this file for a complete archive.\n", "$"+"TOME_BLOB_ROOT")
	}
}
