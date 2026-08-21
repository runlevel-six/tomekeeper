package main

import (
	"flag"
	"fmt"
	"io"
	"strconv"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/jobs"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// refetch asks the origin for pages this archive already has.
//
// The same operation the failed-fetch queue offers per row, as a command, because a
// repair is rarely one article: a site whose image URLs expired takes every article
// from that site with it, and a domain flagged for a browser after the fact takes
// everything fetched before the flag. Clicking a button eight times is not a
// maintenance loop.
//
// **Reports by default and acts only with --yes**, the same way `tome prune` does and
// for a stronger reason: every article here costs somebody else's server a request.
func refetch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("refetch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	apply := fs.Bool("yes", false, "queue the fetches, rather than reporting what would be queued")
	fs.Usage = func() { refetchUsage(stderr) }

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() == 0 {
		refetchUsage(stderr)
		return exitUsage
	}

	ids := make([]store.ArticleID, 0, fs.NArg())
	for _, raw := range fs.Args() {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			fmt.Fprintf(stderr, "tome refetch: %q is not an article id\n", raw)
			return exitUsage
		}
		ids = append(ids, store.ArticleID(n))
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

	// Looked up before anything is queued, so a mistyped id is a message rather than
	// a job that fails somewhere a person is not watching.
	type target struct {
		id      store.ArticleID
		url     string
		hasPage bool
	}
	targets := make([]target, 0, len(ids))
	for _, id := range ids {
		a, err := s.GetArticle(ctx, id)
		if err != nil {
			if store.IsNotFound(err) {
				fmt.Fprintf(stderr, "tome refetch: no article %d\n", id)
				return exitFailure
			}
			fmt.Fprintf(stderr, "tome refetch: %v\n", err)
			return exitFailure
		}
		targets = append(targets, target{id: id, url: a.URLCanonical, hasPage: a.RawBlobPath != ""})
	}

	for _, t := range targets {
		note := ""
		if !t.hasPage {
			// Worth saying: for these there is nothing to replace, so this is an
			// ordinary first fetch and would have happened anyway.
			note = "  (no stored page — this is a first fetch)"
		}
		fmt.Fprintf(stdout, "%d  %s%s\n", t.id, t.url, note)
	}
	fmt.Fprintln(stdout)

	if !*apply {
		fmt.Fprintf(stdout, "%s would be fetched again, one request each. Nothing has been queued.\n",
			plural(len(targets), "page"))
		fmt.Fprintln(stdout, "Run again with --yes to queue them.")
		return exitOK
	}

	// An insert-only client, the same way reextract and serve make one: no queues
	// configured, so this process never runs a job — it only asks for one.
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: log})
	if err != nil {
		log.Error("cannot reach the job queue", "error", err)
		return exitFailure
	}

	for _, t := range targets {
		if err := jobs.EnqueueRefetch(ctx, client, t.id); err != nil {
			fmt.Fprintf(stderr, "tome refetch: %v\n", err)
			return exitFailure
		}
	}

	fmt.Fprintf(stdout, "Queued %s. The worker fetches them under the usual rate limit;\n",
		plural(len(targets), "fetch"))
	fmt.Fprintln(stdout, "each one re-extracts when its page lands.")
	return exitOK
}

func refetchUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: tome refetch [--yes] <article-id>...

Asks the origin for pages this archive already has, replacing what is stored.

For the cases re-extracting cannot fix, because the stored bytes themselves are
wrong: images behind URLs that have since expired, or a page that needed a browser
before its domain was flagged.

Reports what it would fetch and queues nothing unless --yes is given. Every article
costs one request to somebody else's server.
`)
}
