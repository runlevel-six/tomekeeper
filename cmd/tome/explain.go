package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"text/tabwriter"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/db"
	explainer "github.com/runlevel-six/tomekeeper/internal/explain"
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
	username := fs.String("user", "",
		"explain what this reader sees, rather than the household's extraction")

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

	blobs, err := blob.NewFilesystem(cfg.BlobRoot)
	if err != nil {
		fmt.Fprintf(stderr, "tome explain: opening the archive: %v\n", err)
		return exitFailure
	}

	// The same function the web page calls. Two implementations of "what would the
	// ladder do" would drift, and the drift would be invisible: an explanation that
	// no longer describes the extraction is worse than none, because it is believed.
	//
	// The household's view, because that is what a command run by an operator is
	// asking about. `--user` explains what one reader sees instead, which can differ
	// the moment they have written a rule.
	reader := store.HouseholdRule
	if *username != "" {
		id, lookupErr := s.System().LookupUser(ctx, *username)
		if lookupErr != nil {
			return noSuchUser(stderr, "explain", *username, lookupErr)
		}
		reader = id
	}

	report, err := explainer.For(ctx, s, blobs, reader, store.ArticleID(id))
	if err != nil {
		if store.IsNotFound(err) {
			fmt.Fprintf(stderr, "tome explain: no article %d\n", id)
			return exitFailure
		}
		fmt.Fprintf(stderr, "tome explain: %v\n", err)
		return exitFailure
	}

	article, in := report.Article, extract.Input{RawHTML: nil, URL: report.Article.URLCanonical}
	if report.Rule.ContentSelector != "" || len(report.Rule.StripSelectors) > 0 {
		in.Rule = &extract.Rule{
			ContentSelector: report.Rule.ContentSelector,
			StripSelectors:  report.Rule.StripSelectors,
		}
	}
	result, steps, extractErr := report.Result, report.Steps, report.Err

	printExplanation(stdout, article, in, report.RawBytes, result, steps, extractErr, *showBody)

	if extractErr != nil {
		// Not an error in this command: reporting a failure accurately is what it
		// is for. Exit zero so it composes in a loop over the attention queue.
		return exitOK
	}
	return exitOK
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
	if article.BrowserRendered {
		// Worth saying, because it changes what the numbers below mean: the "page" a
		// rendered article was extracted from is the DOM after its scripts ran, not the
		// HTML the server sent, and somebody comparing this against `curl` output would
		// otherwise be comparing two different documents.
		fmt.Fprint(w, " (through a headless browser)")
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
		// "of markup", because the ladder's own line for this rung counts *text* against
		// a 200-character floor. A feed body of 1,427 characters of markup wrapping one
		// image is a handful of characters of text, and two unlabelled counts on adjacent
		// lines read as a contradiction — which is exactly how this command sent somebody
		// hunting for a data-loss bug that did not exist.
		fmt.Fprintf(w, "  feed body: %d characters of markup\n", len(in.FeedBody))
	}

	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  RUNG\tCHARS\tWORDS\tIMAGES\tOUTCOME")
	for _, step := range steps {
		var outcome string
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
