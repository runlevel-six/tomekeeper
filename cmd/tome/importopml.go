package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// importOPML adds subscriptions from an OPML file exported by another reader.
//
// Re-running it is safe: subscriptions are keyed by (user, feed URL), so a
// second import updates titles and categories and creates nothing. That
// property matters more than it sounds — the natural way to recover from a
// half-finished import is to run it again.
func importOPML(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("import-opml", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "parse and report without writing anything")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: tome import-opml [--dry-run] <file.opml>")
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

	// G304 wants a constant path, but a variable one is the entire command: the
	// operator names the OPML file to import. Nothing here is reachable by a
	// remote caller.
	file, err := os.Open(path) //nolint:gosec // the path is the operator's own argument
	if err != nil {
		fmt.Fprintf(stderr, "tome import-opml: %v\n", err)
		return exitUsage
	}
	defer func() { _ = file.Close() }()

	subs, err := feed.ParseOPML(file)
	if err != nil {
		fmt.Fprintf(stderr, "tome import-opml: %v\n", err)
		return exitUsage
	}
	if len(subs) == 0 {
		fmt.Fprintf(stderr, "tome import-opml: %s contains no subscriptions\n", path)
		return exitFailure
	}

	// The dry run stops before the database, so it works without one. Someone
	// evaluating whether to trust this with a long-curated subscription list
	// should be able to see what it would do first.
	if *dryRun {
		fmt.Fprintf(stdout, "%s: %d subscriptions (dry run, nothing written)\n\n", path, len(subs))
		printSubscriptions(stdout, subs)
		return exitOK
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
		fmt.Fprintf(stderr, "tome import-opml: no user %q; run `tome migrate` first\n", cfg.Username)
		return exitFailure
	}

	// Shared with the web upload, so the two cannot disagree about what an
	// import means — in particular that one bad subscription does not cost the
	// rest, and that re-running is safe.
	result := feed.Import(ctx, s, userID, subs)

	for _, f := range result.Failures {
		fmt.Fprintf(stderr, "  ! %s: %v\n", f.FeedURL, f.Err)
	}

	fmt.Fprintf(stdout, "%s: %d added, %d already subscribed\n", path, result.Added, result.Existing)
	if len(result.Failures) > 0 {
		fmt.Fprintf(stdout, "%d subscriptions failed; see the errors above\n", len(result.Failures))
		return exitFailure
	}
	return exitOK
}

func printSubscriptions(w io.Writer, subs []feed.Subscription) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CATEGORY\tTITLE\tFEED URL")
	for _, s := range subs {
		category := s.Category
		if category == "" {
			category = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", category, s.Title, s.FeedURL)
	}
	_ = tw.Flush()
}
