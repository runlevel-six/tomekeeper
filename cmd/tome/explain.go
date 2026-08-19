package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"text/tabwriter"

	"github.com/jackc/pgx/v5"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// explain says what the extraction ladder did to one article, and why.
//
// The missing step in this archive's maintenance loop. That loop runs from the
// attention queue to a domain rule, and between them sits a question the software
// could not answer: *why* did this page produce nothing? "No extractor produced
// acceptable content" is one sentence covering five rungs and four thresholds, which
// is enough to know something is wrong and not enough to do anything about it.
//
// Works from the stored page, so it needs no network and answers for sites that no
// longer exist — which is the whole reason the raw fetch is kept.
func explain(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showBody := fs.Bool("body", false, "print the winning body's opening as well")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: tome explain [--body] <article-id>")
		fmt.Fprintln(stderr, "\nReports what each rung of the extraction ladder produced for one")
		fmt.Fprintln(stderr, "article, and which threshold accepted or rejected it.")
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

	id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintf(stderr, "tome explain: %q is not an article id\n", fs.Arg(0))
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

	article, err := s.GetArticle(ctx, store.ArticleID(id))
	if err != nil {
		fmt.Fprintf(stderr, "tome explain: no article %d\n", id)
		return exitFailure
	}

	blobs, err := blob.NewFilesystem(cfg.BlobRoot)
	if err != nil {
		fmt.Fprintf(stderr, "tome explain: opening the archive: %v\n", err)
		return exitFailure
	}

	raw, err := storedPage(ctx, blobs, article)
	if err != nil {
		fmt.Fprintf(stderr, "tome explain: %v\n", err)
		return exitFailure
	}

	in := extract.Input{RawHTML: raw, URL: article.URLCanonical}

	// The rule that would apply, looked up the same way the worker looks it up, so
	// this explains the extraction that actually happens rather than a hypothetical
	// one with no rule.
	//
	// A missing rule is an answer; a database error is not, and treating the two the
	// same would print a confident explanation of a ladder that never ran this way.
	rule, err := s.System().DomainRuleFor(ctx, hostOfURL(article.URLCanonical))
	switch {
	case err == nil:
		in.Rule = &extract.Rule{
			ContentSelector: rule.ContentSelector,
			StripSelectors:  rule.StripSelectors,
		}
	case !errors.Is(err, pgx.ErrNoRows):
		fmt.Fprintf(stderr, "tome explain: looking up the domain rule: %v\n", err)
		return exitFailure
	}

	// The feed body, for the rung that falls back to it.
	body, err := s.FeedBodyFor(ctx, store.ArticleID(id))
	if err != nil {
		fmt.Fprintf(stderr, "tome explain: looking up the feed body: %v\n", err)
		return exitFailure
	}
	in.FeedBody = body

	result, steps, extractErr := extract.New().Explain(in)

	printExplanation(stdout, article, in, len(raw), result, steps, extractErr, *showBody)

	if extractErr != nil {
		// Not an error in this command: reporting a failure accurately is what it
		// is for. Exit zero so it composes in a loop over the attention queue.
		return exitOK
	}
	return exitOK
}

// storedPage reads and decompresses the page kept for an article.
func storedPage(ctx context.Context, blobs blob.Store, article store.Article) ([]byte, error) {
	if article.RawBlobPath == "" {
		// Not an error: an article can legitimately have no stored page, and what
		// this command should then explain is a ladder with only the feed-body rung
		// available to it.
		return nil, nil
	}

	r, err := blobs.Get(ctx, article.RawBlobPath)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, fmt.Errorf("the stored page %s is missing from the archive; "+
				"the row points at a file that is not there", article.RawBlobPath)
		}
		return nil, fmt.Errorf("reading %s: %w", article.RawBlobPath, err)
	}
	defer func() { _ = r.Close() }()

	compressed, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", article.RawBlobPath, err)
	}

	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("decompressing %s: %w", article.RawBlobPath, err)
	}
	defer func() { _ = gz.Close() }()

	return io.ReadAll(gz)
}

// hostOfURL is the host a domain rule is looked up by.
//
// url.Hostname(), because that is exactly what the extraction worker uses — an
// explanation that resolved the rule differently from the job it is explaining
// would be worse than none.
func hostOfURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func printExplanation(w io.Writer, article store.Article, in extract.Input, rawBytes int,
	result extract.Result, steps []extract.Step, extractErr error, showBody bool,
) {
	fmt.Fprintf(w, "article %d: %s\n", article.ID, article.URLCanonical)
	fmt.Fprintf(w, "  stored page: ")
	switch {
	case article.RawBlobPath == "":
		fmt.Fprintln(w, "none — nothing was fetched")
	case rawBytes < 1024:
		// Rounding a tiny page to "0 KB" reads as a bug in this command rather
		// than as the finding it usually is: an empty page that fetched fine.
		fmt.Fprintf(w, "%s (%d bytes uncompressed)\n", article.RawBlobPath, rawBytes)
	default:
		fmt.Fprintf(w, "%s (%d KB uncompressed)\n", article.RawBlobPath, rawBytes/1024)
	}
	fmt.Fprintf(w, "  fetch: %s", article.FetchStatus)
	if article.FetchError != "" {
		fmt.Fprintf(w, " — %s", article.FetchError)
	}
	fmt.Fprintln(w)
	if in.Rule != nil && in.Rule.ContentSelector != "" {
		fmt.Fprintf(w, "  domain rule: %s", in.Rule.ContentSelector)
		if n := len(in.Rule.StripSelectors); n > 0 {
			fmt.Fprintf(w, " (and %d strip selector(s))", n)
		}
		fmt.Fprintln(w)
	}
	if in.FeedBody != "" {
		fmt.Fprintf(w, "  feed body: %d characters available\n", len(in.FeedBody))
	}

	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  RUNG\tCHARS\tWORDS\tIMAGES\tOUTCOME")
	for _, step := range steps {
		outcome := "-"
		switch {
		case !step.Ran:
			outcome = "skipped"
		case step.Accepted:
			outcome = "ACCEPTED"
		case step.Rung == "page":
			outcome = "measured"
		default:
			outcome = "rejected"
		}
		fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%s: %s\n",
			step.Rung, step.Chars, step.Words, step.Images, outcome, step.Why)
	}
	_ = tw.Flush()

	fmt.Fprintln(w)
	if extractErr != nil {
		fmt.Fprintf(w, "Result: no body. %v\n", extractErr)
		fmt.Fprintln(w, "\nWhat usually fixes this: a domain rule naming the element that holds")
		fmt.Fprintln(w, "the body, which overrides the length and ratio checks above.")
		host := hostOfURL(article.URLCanonical)
		fmt.Fprintf(w, "\n  tome domain-rule set %s --selector '<css>'\n", host)
		fmt.Fprintf(w, "  tome reextract --target-version 0 --domain %s\n", host)
		fmt.Fprintln(w, "\nReextraction works from the stored page, so it costs no requests.")
		fmt.Fprintln(w, "--target-version 0 because it selects on extractor version, and a bare run")
		fmt.Fprintln(w, "finds nothing once every body on the site is already current; --domain")
		fmt.Fprintln(w, "because a rule can only affect the one site.")
		fmt.Fprintln(w, "\nIf the page renders its article with JavaScript, there is nothing in the")
		fmt.Fprintln(w, "stored copy to find and no rule will help.")
		return
	}

	fmt.Fprintf(w, "Result: %s, %d words\n", result.Name, result.WordCount)
	if showBody {
		fmt.Fprintln(w)
		fmt.Fprintln(w, first(result.Text, 600))
	}
}

func first(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
