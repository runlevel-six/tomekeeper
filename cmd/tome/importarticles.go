package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/exchange"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// importArticles imports a reading library exported from another system.
//
// Two passes over the file, always, even without --dry-run. The first reads the
// whole export and reports what importing it would do; only then does the second
// write. That costs one extra read of a file measured in megabytes and buys two
// things worth more: a truncated or corrupt export fails before anything has been
// written, and nobody is ever surprised by what an import did to a library they
// have been keeping for ten years.
func importArticles(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "report what would be imported and write nothing")
	format := fs.String("format", "",
		"source format ("+strings.Join(exchange.ImporterNames(), ", ")+"); detected when omitted")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: tome import [--dry-run] [--format NAME] <export-file>")
		fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}
	path := fs.Arg(0)

	imp, code := importerFor(*format, path, stderr)
	if code != exitOK {
		return code
	}

	cfg, log, code := loadConfigAndLogger(stderr)
	if code != exitOK {
		return code
	}

	ctx, stop := signalContext()
	defer stop()

	// The database is required even for a dry run, unlike the OPML import's. The
	// three numbers that make this report worth reading — new, already imported,
	// already in the archive by another route — are all questions about the archive,
	// and a report that could not answer them would be a list of what is in a file
	// the operator already has.
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("cannot reach the database", "error", err)
		return exitFailure
	}
	defer pool.Close()

	s := store.New(pool)

	userID, err := s.System().LookupUser(ctx, cfg.Username)
	if err != nil {
		fmt.Fprintf(stderr, "tome import: no user %q; run `tome migrate` first\n", cfg.Username)
		return exitFailure
	}

	file, err := openExport(path, stderr)
	if err != nil {
		return exitUsage
	}
	report, err := exchange.Inspect(ctx, s, userID, imp, exchange.Source{Path: path, Reader: file})
	_ = file.Close()
	if err != nil {
		fmt.Fprintf(stderr, "tome import: %v\n", err)
		return exitFailure
	}

	printImportReport(stdout, report, *dryRun)

	if *dryRun {
		return exitOK
	}

	// A second reader over the same file. Reopened rather than rewound so that the
	// two passes cannot share a position, which is the kind of bug that imports
	// half a library and reports all of it.
	file, err = openExport(path, stderr)
	if err != nil {
		return exitUsage
	}
	defer func() { _ = file.Close() }()

	applied, err := exchange.Apply(ctx, s, userID, imp, exchange.Source{Path: path, Reader: file})
	if err != nil {
		fmt.Fprintf(stderr, "tome import: %v\n", err)
		return exitFailure
	}

	printImportResult(stdout, stderr, applied)

	if applied.Written != nil && len(applied.Written.Failures) > 0 {
		return exitFailure
	}
	return exitOK
}

// importerFor picks the adapter, by name or by looking at the file.
func importerFor(format, path string, stderr io.Writer) (exchange.Importer, int) {
	if format != "" {
		imp, ok := exchange.ImporterNamed(format)
		if !ok {
			fmt.Fprintf(stderr, "tome import: unknown format %q; this build reads %s\n",
				format, strings.Join(exchange.ImporterNames(), ", "))
			return nil, exitUsage
		}
		return imp, exitOK
	}

	imp, err := exchange.DetectImporter(path)
	if err != nil {
		fmt.Fprintf(stderr, "tome import: %v\n", err)
		return nil, exitUsage
	}
	if imp == nil {
		fmt.Fprintf(stderr,
			"tome import: %s is not a format this build recognizes (%s); name one with --format\n",
			path, strings.Join(exchange.ImporterNames(), ", "))
		return nil, exitUsage
	}
	return imp, exitOK
}

func openExport(path string, stderr io.Writer) (*os.File, error) {
	// G304 wants a constant path, but a variable one is the entire command: the
	// operator names the export to import, and nothing here is remotely reachable.
	file, err := os.Open(path) //nolint:gosec // the path is the operator's own argument
	if err != nil {
		fmt.Fprintf(stderr, "tome import: %v\n", err)
		return nil, err
	}
	return file, nil
}

// printImportReport prints what the file holds and what importing it would do.
//
// A table of numbers with the consequential ones annotated. The annotations are
// the point: "43 without a body" invites the wrong conclusion on its own, and "42
// of those are wallabag's own fetch-failure placeholder, which this archive will
// try to fetch for itself" is the fact that makes the number good news.
func printImportReport(w io.Writer, r exchange.Report, dryRun bool) {
	heading := fmt.Sprintf("%s: %d records from %s", r.Path, r.Records, r.Source)
	if dryRun {
		heading += " (dry run, nothing written)"
	}
	fmt.Fprintln(w, heading)
	fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	line := func(label string, n int, note string) {
		if note != "" {
			fmt.Fprintf(tw, "  %s\t%d\t%s\n", label, n, note)
			return
		}
		fmt.Fprintf(tw, "  %s\t%d\t\n", label, n)
	}

	line("new", r.New, "")
	line("already imported", r.AlreadyImported, "")

	duplicates := ""
	if r.DuplicateURLs > 0 {
		duplicates = "already in the archive; the import adds a reference, not a copy"
	}
	line("duplicate URLs", r.DuplicateURLs, duplicates)

	withoutBody := ""
	switch {
	case r.PlaceholderBodies > 0:
		withoutBody = fmt.Sprintf(
			"%d of these hold %s's own fetch-failure message; this archive will fetch them itself",
			r.PlaceholderBodies, r.Source)
	case r.WithoutBody > 0:
		withoutBody = "queued for this archive to fetch"
	}
	line("without a body", r.WithoutBody, withoutBody)

	line("with images", r.WithImages, imageNote(r))

	line("tags", r.Tags, "")
	line("highlights", r.Highlights, "")

	if len(r.Problems) > 0 {
		line("unreadable records", len(r.Problems), "listed below; the rest still import")
	}
	_ = tw.Flush()

	if len(r.Problems) > 0 {
		fmt.Fprintln(w)
		for _, p := range r.Problems {
			fmt.Fprintf(w, "  ! record %d: %v\n", p.Record, p.Err)
		}
	}

	if r.Images.Fetchable > 0 {
		fmt.Fprintf(w, "\nImages are fetched by the worker after the import, not now. "+
			"Until it gets to them, articles show their text with the images missing.\n")
	}
	if r.Images.InSourceStorage > 0 {
		fmt.Fprintf(w, "\n%s live inside the %s installation rather than at the sites they "+
			"came from, because it had image downloading switched on. This archive cannot reach "+
			"them, so those articles arrive without their pictures. The original sites are the "+
			"other way to get them.\n",
			plural(r.Images.InSourceStorage, "image"), r.Source)
	}
}

// plural is a count with its noun, so a report does not say "1 images".
//
// The sibilant cases take -es, which is not decoration: `tome refetch` shipped saying
// "Queued 8 fetchs" because a bare -s is wrong after ch, sh, s, x and z. Anything
// irregular should be written out at the call site rather than guessed at here.
func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	for _, ending := range []string{"ch", "sh", "s", "x", "z"} {
		if strings.HasSuffix(unit, ending) {
			return fmt.Sprintf("%d %ses", n, unit)
		}
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// imageNote says what the images in a library will cost and what will be missing.
func imageNote(r exchange.Report) string {
	c := r.Images
	if c.Total() == 0 {
		return ""
	}

	parts := []string{plural(c.Fetchable, "image") + " to fetch and archive"}
	if c.InSourceStorage > 0 {
		parts = append(parts,
			fmt.Sprintf("%d held inside the %s installation and unreachable from here",
				c.InSourceStorage, r.Source))
	}
	if c.SelfContained > 0 {
		parts = append(parts, fmt.Sprintf("%d already inline", c.SelfContained))
	}
	if c.Unusable > 0 {
		parts = append(parts, fmt.Sprintf("%d not usable addresses", c.Unusable))
	}
	return strings.Join(parts, ", ")
}

// printImportResult says what the import actually did.
func printImportResult(stdout, stderr io.Writer, r exchange.Report) {
	written := r.Written
	if written == nil {
		return
	}

	fmt.Fprintf(stdout, "\nimported %d articles: %d bodies stored, %d queued for fetching",
		written.Articles, written.Bodies, written.QueuedForFetch)
	if written.TagsAdded > 0 || written.HighlightsAdded > 0 {
		fmt.Fprintf(stdout, ", %d tags, %d highlights", written.TagsAdded, written.HighlightsAdded)
	}
	fmt.Fprintln(stdout)

	if r.AlreadyImported > 0 {
		fmt.Fprintf(stdout, "%d records were already imported and were left alone.\n", r.AlreadyImported)
	}

	for _, f := range written.Failures {
		fmt.Fprintf(stderr, "  ! record %d: %v\n", f.Record, f.Err)
	}
	if len(written.Failures) > 0 {
		fmt.Fprintf(stdout,
			"%d records failed to import. Re-running is safe and will retry them.\n",
			len(written.Failures))
	}
}
