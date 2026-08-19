package exchange

import (
	"context"
	"fmt"
	"strings"

	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/store"
	"github.com/runlevel-six/tomekeeper/internal/urlcanon"
)

// Report is what an import says about an export before and after touching it.
//
// The same type serves both passes, because the promise of a dry run is that it
// describes the run that follows. A report with different fields in each mode
// would be a different claim, and the point of the report is that somebody can
// trust it with a library they have been adding to for ten years.
type Report struct {
	// Source is the adapter that read the file, and Path is the file.
	Source string
	Path   string

	// Records is every record the file contained, including the ones counted as
	// problems below.
	Records int

	// New is records this reader has not imported before.
	New int

	// AlreadyImported is records whose source id is already in import_records.
	// Re-running an import is expected to produce a report that is almost entirely
	// this.
	AlreadyImported int

	// DuplicateURLs is records whose canonical URL is already in the archive by
	// another route — carried by a subscribed feed, or saved by hand. They are not
	// skipped: the import becomes another reference to the article that is already
	// there, which is the whole reason the article rather than the feed item is the
	// root entity. Counted because "why did importing 385 articles add 340" has a
	// good answer and needs somewhere to be seen.
	DuplicateURLs int

	// WithoutBody is records the source had no usable body for. Includes the ones
	// carrying the source's own fetch-failure placeholder, which is why this is
	// usually larger than a reader expects.
	WithoutBody int

	// PlaceholderBodies is how many of WithoutBody were a placeholder rather than
	// an empty field. Separated because it is the number that changes what somebody
	// believes about their own library: these are articles their reader shows as
	// saved and has never actually held.
	PlaceholderBodies int

	// Images is every image reference in the library, classified. What it is for
	// is the question "what will my archive look like when this finishes", which
	// images decide: they are the bulk of an archive's bytes and the whole of its
	// appearance.
	Images ImageCensus

	// WithImages is how many records carry any image at all.
	WithImages int

	// Highlights and Tags are what came with the library.
	Highlights int
	Tags       int

	// Problems are records that could not be read, with their position in the
	// file. A record here is one the import skipped; the rest still import.
	Problems []Problem

	// Written is filled in by Apply: what actually changed.
	Written *Written
}

// Problem is one record the adapter could not turn into an article.
type Problem struct {
	// Record is the 1-based position in the file, which is what somebody needs to
	// find it in an export they cannot otherwise search.
	Record int
	Err    error
}

// Written is what an import changed.
type Written struct {
	Articles        int
	Bodies          int
	QueuedForFetch  int
	TagsAdded       int
	HighlightsAdded int
	Failures        []Problem
}

// Inspect reads an export without writing anything, and reports what importing it
// would do.
//
// This runs against the database — it has to, because "already imported" and
// "already in the archive" are questions only the archive can answer, and they are
// the two numbers somebody most wants before they commit. It writes nothing.
//
// A nil store is allowed: the report then describes the file alone, which is what
// makes `--dry-run` usable before a database exists.
func Inspect(ctx context.Context, s *store.Store, userID store.UserID,
	imp Importer, src Source,
) (Report, error) {
	report := Report{Source: imp.Name(), Path: src.Path}

	for article, err := range imp.Import(ctx, src) {
		if err != nil {
			// A fatal read — a truncated file, or a syntax error that leaves the
			// position unknown — is the whole file's problem, not one record's. The
			// adapter signals it by yielding no article.
			if article == nil && isFatal(err) {
				return report, err
			}
			report.Problems = append(report.Problems, Problem{Record: report.Records + 1, Err: err})
			report.Records++
			continue
		}
		report.Records++

		if err := ctx.Err(); err != nil {
			return report, err
		}

		count(&report, article)

		if s == nil {
			continue
		}

		_, imported, err := s.ImportedArticle(ctx, userID, article.SourceName, article.SourceID)
		if err != nil {
			return report, err
		}
		if imported {
			report.AlreadyImported++
			continue
		}
		report.New++

		canonical, err := canonicalOf(article)
		if err != nil {
			// A URL the archive cannot key on. Reported as a problem rather than
			// counted as importable, because it is a record that will not import.
			report.Problems = append(report.Problems, Problem{Record: report.Records, Err: err})
			report.New--
			continue
		}
		_, have, err := s.ArticleVisibleByURL(ctx, userID, canonical)
		if err != nil {
			return report, err
		}
		if have {
			report.DuplicateURLs++
		}
	}

	return report, nil
}

// count tallies everything about a record that needs no database.
func count(r *Report, a *Article) {
	if a.ContentHTML == "" {
		r.WithoutBody++
		if a.PlaceholderBody {
			r.PlaceholderBodies++
		}
	}

	images := censusOf(a.ContentHTML)
	if images.Total() > 0 {
		r.WithImages++
	}
	r.Images.Add(images)

	r.Highlights += len(a.Highlights)
	r.Tags += len(a.Tags)
}

// Apply imports an export.
//
// One record's failure does not stop the run. That is deliberate and it is the
// same judgment the OPML import makes: in a library of thousands, the useful
// outcome of one unreadable record is a line in a report, not the loss of
// everything after it. A failure that ends the file still stops, because past that
// point there is nothing to read.
//
// Every write is idempotent, so re-running after any failure — including a
// disconnected database halfway through — finishes the job rather than duplicating
// what was done. That property is what makes the "run it again" recovery honest.
func Apply(ctx context.Context, s *store.Store, userID store.UserID,
	imp Importer, src Source,
) (Report, error) {
	e := extract.New()

	report := Report{Source: imp.Name(), Path: src.Path, Written: &Written{}}

	for article, err := range imp.Import(ctx, src) {
		if err != nil {
			if article == nil && isFatal(err) {
				return report, err
			}
			report.Problems = append(report.Problems, Problem{Record: report.Records + 1, Err: err})
			report.Records++
			continue
		}
		report.Records++

		if err := ctx.Err(); err != nil {
			return report, err
		}
		count(&report, article)

		params, err := paramsFor(e, article)
		if err != nil {
			report.Problems = append(report.Problems, Problem{Record: report.Records, Err: err})
			continue
		}

		result, err := s.ImportArticle(ctx, userID, params)
		if err != nil {
			// Recorded and carried on. The record can be retried by re-running,
			// which is cheaper for the operator than a run that abandons the
			// remaining library because one row failed.
			report.Written.Failures = append(report.Written.Failures,
				Problem{Record: report.Records, Err: err})
			continue
		}

		switch {
		case result.AlreadyImported:
			report.AlreadyImported++
		default:
			report.New++
			report.Written.Articles++
			if result.ArticleExisted {
				report.DuplicateURLs++
			}
		}
		if result.BodyStored {
			report.Written.Bodies++
		}
		if params.ContentHTML == "" && !result.AlreadyImported {
			// No body from the source, so the article sits at fetch_status 'pending'
			// and this archive will try the page itself. Counted because it is the
			// most encouraging number in the report: these are the articles the
			// source had lost.
			//
			// Only for records imported by this run. A re-import queues nothing —
			// the article is already waiting, or has since been fetched — and saying
			// otherwise would credit the run with work it did not do.
			report.Written.QueuedForFetch++
		}
		report.Written.TagsAdded += result.TagsAdded
		report.Written.HighlightsAdded += result.HighlightsAdded
	}

	return report, nil
}

// paramsFor turns one imported article into what the store needs to write.
//
// This is where an imported body is made safe. The HTML arrives as whatever
// another system stored — for a decade-old save, markup that predates every
// mitigation the web has since acquired — and it is going into a body this archive
// renders as trusted HTML on the reader's own origin. It goes through the same
// sanitizer and the same URL resolution as everything the extraction ladder
// produces, because a second policy for imports would be a second policy to keep
// in step, and the cost of them drifting is a stored script running with the
// reader's session.
func paramsFor(e *extract.Extractor, a *Article) (store.ImportParams, error) {
	canonical, err := canonicalOf(a)
	if err != nil {
		return store.ImportParams{}, err
	}

	p := store.ImportParams{
		SourceName:   a.SourceName,
		SourceID:     a.SourceID,
		URLCanonical: canonical,
		URLOriginal:  a.URL,
		Title:        a.Title,
		Author:       a.Author,
		SiteName:     a.SiteName,
		Language:     a.Language,
		PublishedAt:  a.PublishedAt,
		SavedAt:      a.SavedAt,
		Read:         a.Read,
		Starred:      a.Starred,
		Tags:         a.Tags,

		// Passed through rather than assumed. A record that says where its body came
		// from is describing an archive being restored, and a restore has to
		// reproduce what was there — including whether the body can be re-extracted
		// later. A record that says nothing is another system's library, and gets
		// the immutable import treatment.
		Extractor:        a.Extractor,
		ExtractorVersion: a.ExtractorVersion,
		ContentOrigin:    a.ContentOrigin,
		Immutable:        a.Immutable,
	}

	for _, h := range a.Highlights {
		p.Highlights = append(p.Highlights, store.ImportHighlight{
			Quote: h.Quote, Note: h.Note, CreatedAt: h.CreatedAt,
		})
	}

	if a.ContentHTML != "" {
		// Resolved against the article's own URL, which is why this uses the
		// resolved address where the source recorded one: relative references in
		// the body were written relative to the page as fetched.
		base := a.ResolvedURL
		if base == "" {
			base = a.URL
		}

		body := e.CleanImported(a.ContentHTML, base)
		p.ContentHTML = body.HTML
		p.ContentText = body.Text
		p.WordCount = body.WordCount

		// Sanitizing can empty a body that was nothing but markup this archive will
		// not store — a save that captured only an iframe, say. Treated as no body,
		// so the article is queued for a fetch instead of storing an empty
		// immutable row that nothing could ever replace.
		if strings.TrimSpace(p.ContentHTML) == "" {
			p.ContentHTML, p.ContentText, p.WordCount = "", "", 0
		}
	}

	return p, nil
}

// canonicalOf is the URL the archive keys an imported article on.
//
// The resolved URL where the source recorded one: it is the address the source
// actually fetched, and it is the one that will match an article a feed brought.
// Canonicalization then strips the tracking parameters that make two saves of one
// page look like two pages.
func canonicalOf(a *Article) (string, error) {
	raw := strings.TrimSpace(a.ResolvedURL)
	if raw == "" {
		raw = strings.TrimSpace(a.URL)
	}

	canonical, err := urlcanon.Canonicalize(raw)
	if err != nil {
		return "", fmt.Errorf("%q cannot be used as an article URL: %w", raw, err)
	}
	return canonical, nil
}
