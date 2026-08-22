package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// audit reports bodies that may not be what they claim to be.
//
// The failed-fetch queue answers "what did not arrive". This answers the harder
// question underneath it — "what arrived and is wrong" — which nothing asked before a
// re-fetch stored a cookie consent dialog as a 410-word article and took it out of
// the queue by succeeding.
//
// **Read-only, and never a gate.** Every lens here was measured against a real archive
// before being written, and none is precise enough to act on its own: the title lens
// flags seven bodies out of 2,211 and about three of them are real. That is a fine
// list to read and a terrible one to delete from. So this prints and stops.
func audit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	limit := fs.Int("limit", 50, "most findings to report per lens")
	fs.Usage = func() { auditUsage(stderr) }

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 0 {
		auditUsage(stderr)
		return exitUsage
	}
	if *limit < 1 {
		fmt.Fprintln(stderr, "tome audit: --limit must be at least 1")
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
	findings := 0

	suspect, err := s.SuspectBodies(ctx, *limit)
	if err != nil {
		fmt.Fprintf(stderr, "tome audit: %v\n", err)
		return exitFailure
	}
	findings += len(suspect)
	section(stdout, "Bodies that say nothing their title mentions",
		`Extraction chose a block of the page and may have chosen the wrong one. A cookie
notice, a sign-in wall or a related-articles rail reads as prose and passes every
threshold the ladder enforces. Expect false positives: a link roundup, a podcast page,
a title in another language.`, len(suspect))
	for _, b := range suspect {
		fmt.Fprintf(stdout, "  %-7d %-12s %5s  %s\n", b.ArticleID, b.Extractor,
			fmt.Sprint(b.WordCount), truncate(b.Title, 58))
		fmt.Fprintf(stdout, "          %s\n", b.URL)
	}

	shared, err := s.SharedBodies(ctx, *limit)
	if err != nil {
		fmt.Fprintf(stderr, "tome audit: %v\n", err)
		return exitFailure
	}
	findings += len(shared)
	section(stdout, "Bodies that more than one article shares",
		`Two articles do not coincidentally have the same prose. What they share is
usually a wall — a consent gate, a sign-in page — stored as though it were content.
Reposts of the same story are the ordinary innocent case.`, len(shared))
	for _, b := range shared {
		ids := make([]string, 0, len(b.ArticleIDs))
		for _, id := range b.ArticleIDs {
			ids = append(ids, fmt.Sprint(id))
		}
		note := ""
		if b.Immutable {
			// Worth saying, because it changes what can be done about it: an imported
			// body is never re-extracted, so the remedy is not a domain rule.
			note = "  [imported — not re-extractable]"
		}
		fmt.Fprintf(stdout, "  %-24s %5d words  %s%s\n",
			strings.Join(ids, ","), b.WordCount, strings.Join(b.Hosts, ", "), note)
		fmt.Fprintf(stdout, "          %s\n", truncate(b.Opening, 74))
	}

	titles, err := s.PlaceholderTitles(ctx, *limit)
	if err != nil {
		fmt.Fprintf(stderr, "tome audit: %v\n", err)
		return exitFailure
	}
	findings += len(titles)
	section(stdout, "Titles that are not titles",
		`A URL or an encoded filename kept as a title, which an import does when its
source had none. One with a body gets a real title from the next `+"`tome reextract`"+`;
one without has no page to take a title from and needs a fetch first.`, len(titles))
	for _, t := range titles {
		state := "needs a fetch"
		if t.HasBody {
			state = "reextract fixes"
		}
		fmt.Fprintf(stdout, "  %-7d %-16s %s\n", t.ArticleID, state, truncate(t.Title, 62))
	}

	fmt.Fprintln(stdout)
	if findings == 0 {
		fmt.Fprintln(stdout, "Nothing to look at.")
		return exitOK
	}
	fmt.Fprintf(stdout, "%s across three lenses. Nothing has been changed — every one of\n",
		plural(findings, "finding"))
	fmt.Fprintln(stdout, "these is a judgement call, and some of them are meant to be false alarms.")
	return exitOK
}

// section prints a heading, its explanation, and what to expect of it.
//
// The explanation is printed even when the lens found nothing, because "0 findings"
// only means something if you know what was looked for.
func section(w io.Writer, title, why string, n int) {
	fmt.Fprintf(w, "\n%s\n%s\n", title, strings.Repeat("─", len([]rune(title))))
	for _, line := range strings.Split(strings.TrimSpace(why), "\n") {
		fmt.Fprintf(w, "%s\n", line)
	}
	if n == 0 {
		fmt.Fprintln(w, "\n  none")
		return
	}
	fmt.Fprintln(w)
}

func truncate(s string, max int) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max-1]) + "…"
}

func auditUsage(w io.Writer) {
	fmt.Fprint(w, `Usage: tome audit [--limit N]

Reports stored bodies that may not be what they claim to be, through three lenses:
bodies sharing no word with their title, bodies more than one article shares, and
titles that are URLs.

Read-only. Every lens is deliberately imprecise enough that acting on it
automatically would throw away real articles, so this prints and changes nothing.

Flags:
  --limit N   most findings to report per lens (default 50)
`)
}
