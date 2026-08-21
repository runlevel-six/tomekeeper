package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// prune releases the bodies of articles nothing points at any more.
//
// The residue unsubscribing leaves. Retention cannot reach it — ExpirableArticles
// requires an article to have been *read*, so one that arrived, was never opened, and
// then lost its feed is never expirable at any setting — and unsubscribing
// deliberately deletes no articles, because re-subscribing relinks them by canonical
// URL. So nothing has ever collected them.
//
// **Reports by default and acts only when told.** The opposite convention to
// `reextract --dry-run`, deliberately: re-extracting is free and reversible, while
// this releases bytes that then have to be fetched again. The safe answer is the one
// you get by accident.
func prune(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("yes", false, "release the bodies, rather than reporting what would be released")
	limit := fs.Int("limit", 500, "consider at most this many articles")
	verbose := fs.Bool("list", false, "name every article rather than only counting them")
	fs.Usage = func() { pruneUsage(stderr) }

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		return usageError(stderr, "prune", fs.Arg(0))
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

	candidates, err := s.PrunableArticles(ctx, *limit)
	if err != nil {
		fmt.Fprintf(stderr, "tome prune: %v\n", err)
		return exitFailure
	}

	if len(candidates) == 0 {
		fmt.Fprintln(stdout, "Nothing to prune: every stored article is either referenced by a feed or has been acted on.")
		return exitOK
	}

	var total int64
	for _, c := range candidates {
		total += c.Bytes
	}

	if *verbose {
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		for _, c := range candidates {
			title := c.Title
			if title == "" {
				title = "(untitled)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", humanBytes(c.Bytes), title, c.URL)
		}
		_ = tw.Flush()
		fmt.Fprintln(stdout)
	}

	if !*apply {
		fmt.Fprintf(stdout,
			"%s across %s would be released. Nothing has changed.\n",
			humanBytes(total), plural(len(candidates), "article"))
		fmt.Fprintln(stdout, "Run again with --yes to release them, or --list to see which.")
		fmt.Fprintln(stdout)
		// Said plainly, because "prune" sounds more final than this is and somebody
		// deciding whether to run it deserves to know what survives.
		fmt.Fprintln(stdout, "The articles themselves are kept: this releases stored bodies and the")
		fmt.Fprintln(stdout, "images no other article holds, exactly as retention does. Nothing")
		fmt.Fprintln(stdout, "starred, saved, or read is ever a candidate.")
		return exitOK
	}

	blobs, err := blob.NewFilesystem(cfg.BlobRoot)
	if err != nil {
		log.Error("cannot reach the archive", "error", err)
		return exitFailure
	}

	var (
		freed  int64
		pruned int
		failed int
	)
	for _, c := range candidates {
		expired, err := s.ExpireArticle(ctx, c.ArticleID)
		if err != nil {
			// One article's failure is not the run's. Logged and counted, because a
			// run that stopped at the first problem would leave the archive in a
			// state nobody asked for and no report to explain it.
			log.Error("could not release an article", "article_id", c.ArticleID, "error", err)
			failed++
			continue
		}

		// The files, after the database has committed. ExpireArticle returns the
		// paths rather than unlinking them for exactly this reason: a file removed
		// for a transaction that then rolls back is unrecoverable, while a row that
		// outlives its file is merely wrong in a way that can be fixed.
		paths := expired.AssetPaths
		if expired.RawPath != "" {
			paths = append(paths, expired.RawPath)
		}
		for _, path := range paths {
			if err := blobs.Delete(ctx, path); err != nil && !errors.Is(err, blob.ErrNotFound) {
				log.Warn("could not remove a released file", "path", path, "error", err)
			}
		}

		freed += expired.BodyBytes + expired.AssetBytes
		pruned++
	}

	fmt.Fprintf(stdout, "Released %s across %s.\n", humanBytes(freed), plural(pruned, "article"))
	if failed > 0 {
		fmt.Fprintf(stdout, "%s could not be released; the log says why.\n", plural(failed, "article"))
		return exitFailure
	}
	return exitOK
}

func pruneUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: tome prune [--yes] [--list] [--limit N]

Releases the stored bodies of articles no feed references and nobody has acted on —
the residue unsubscribing leaves, which retention cannot reach because it only ever
expires articles that were read.

Reports what it would release and changes nothing unless --yes is given.

The articles themselves are kept, as are anything starred, saved, read, or imported.
`)
}
