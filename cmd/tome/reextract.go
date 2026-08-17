package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/jobs"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// reextract queues re-extraction of articles whose bodies were produced by an
// older extractor version.
//
// This is the payoff for keeping the raw fetch. Because it was kept, an
// improvement to extraction can be applied to the whole archive — including
// articles from a decade ago, and including articles whose sites no longer
// exist — without asking a single server for anything.
//
// --domain exists because the common reason to reprocess is a domain rule that
// was just written for one badly-extracting site. Without it, fixing one site
// means re-extracting everything, which at a large archive is hours of work to
// correct a handful of articles.
func reextract(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reextract", flag.ContinueOnError)
	fs.SetOutput(stderr)

	sinceVersion := fs.String("since-version", extract.Version,
		"reprocess articles whose body came from an extractor version other than this")
	domain := fs.String("domain", "",
		"only reprocess articles from this host and its subdomains (default: every host)")
	limit := fs.Int("limit", 0, "stop after queueing this many articles (0 means no limit)")
	dryRun := fs.Bool("dry-run", false, "report what would be queued without queueing it")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: tome reextract [--since-version V] [--domain HOST] [--limit N] [--dry-run]")
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

	// An insert-only client: this command queues work for the worker pool and
	// runs none of it itself, so a reprocess of the whole archive is subject
	// to the same concurrency and rate limits as everything else.
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: log})
	if err != nil {
		log.Error("cannot open the job queue", "error", err)
		return exitFailure
	}

	total, err := queueReextractions(ctx, s, client, *sinceVersion, *domain, *limit, *dryRun, stdout)
	if err != nil {
		log.Error("reextract failed", "error", err)
		return exitFailure
	}

	scope := ""
	if *domain != "" {
		scope = " under " + *domain
	}
	noun := "articles"
	if total == 1 {
		noun = "article"
	}

	switch {
	case total == 0 && *domain != "":
		// Two quite different situations, and the reader needs to know which:
		// nothing to reprocess, or nothing archived from that host at all — often
		// a typo in the domain.
		fmt.Fprintf(stdout, "nothing to do: no article%s has a mutable body at a version other than %s\n",
			scope, *sinceVersion)
		fmt.Fprintln(stdout, "if that is unexpected, check the spelling; `tome archive stats` lists what is stored")
	case total == 0:
		fmt.Fprintf(stdout, "nothing to do: every mutable body is already at version %s\n", *sinceVersion)
	case *dryRun:
		fmt.Fprintf(stdout, "%d %s%s would be re-extracted (dry run, nothing queued)\n", total, noun, scope)
	default:
		fmt.Fprintf(stdout, "queued %d %s%s for re-extraction; run `tome worker` to process them\n",
			total, noun, scope)
	}
	return exitOK
}

// queueReextractions walks the candidates in batches and enqueues each one.
func queueReextractions(
	ctx context.Context,
	s *store.Store,
	client *river.Client[pgx.Tx],
	version string,
	domain string,
	limit int,
	dryRun bool,
	stdout io.Writer,
) (int, error) {
	const batch = 500

	var (
		total  int
		cursor store.ArticleID
	)

	for {
		// Candidates are selected by the store with `NOT immutable` in the
		// WHERE clause rather than filtered here. That is deliberate: the
		// acceptance criterion is that imported bodies are *provably* skipped,
		// and a WHERE clause is a proof while a conditional in a loop is a
		// promise.
		candidates, err := s.System().ReextractCandidates(ctx, version, domain, cursor, batch)
		if err != nil {
			return total, err
		}
		if len(candidates) == 0 {
			return total, nil
		}

		for _, c := range candidates {
			cursor = c.ArticleID

			if limit > 0 && total >= limit {
				return total, nil
			}
			if !dryRun {
				// Forced: these articles all have a current body already, and
				// without it the worker would see one and skip.
				if err := jobs.EnqueueExtraction(ctx, client, c.ArticleID, true); err != nil {
					return total, err
				}
			}
			total++
		}

		if total > 0 && total%2000 == 0 {
			fmt.Fprintf(stdout, "  … %d articles so far\n", total)
		}
	}
}
