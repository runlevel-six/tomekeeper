// Package explain answers "why does this article look like this" for one reader.
//
// The extraction ladder is the least legible thing in the archive: six rungs, each
// with thresholds, any of which can quietly decide what somebody reads. `tome
// explain` has always been able to say which rung won and why, and it needed a
// terminal and — on Kubernetes — permission to exec into a pod. Once a reader can
// write their own rules, the person who most needs that answer is the one least
// likely to have either.
//
// So the logic lives here rather than in the command, and both the command and the
// page call it. Two implementations of "what would the ladder do" would drift, and
// the drift would be invisible: an explanation that no longer describes the
// extraction is worse than none, because it is believed.
//
// It runs the ladder over the page already on disk and touches no network — the
// same property that lets `tome reextract` reprocess a decade of articles without
// asking anyone for anything.
package explain

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// Report is everything needed to say why a body looks the way it does.
type Report struct {
	Article store.Article

	// Rule is what applies to this article for this reader, which may be their own
	// or the household's. FromReader says which, and that is the first thing
	// somebody debugging their own selector needs to know.
	Rule store.EffectiveRule

	// RawBytes is the size of the stored page the ladder ran over. Zero means
	// there is none, and that a body could only have come from the feed.
	RawBytes int

	// Result is the rung that won, and Steps is what every rung produced.
	Result extract.Result
	Steps  []extract.Step

	// Err is the ladder's own verdict when nothing was acceptable. Held rather
	// than returned, because "no rung produced anything" is the explanation in
	// exactly the case somebody is asking.
	Err error

	// Stored describes the body actually on record, which is not always what the
	// ladder would produce now — an older extractor version, a rule written since,
	// or a copy promoted by hand. The difference between Stored and Result is
	// usually the whole answer.
	Stored      store.Content
	HasStored   bool
	StoredStale bool
}

// For runs the ladder over one article's stored page as one reader.
func For(
	ctx context.Context,
	s *store.Store,
	blobs blob.Store,
	reader store.UserID,
	id store.ArticleID,
) (Report, error) {
	article, err := s.GetArticle(ctx, id)
	if err != nil {
		return Report{}, err
	}

	raw, err := storedPage(ctx, blobs, article)
	if err != nil {
		return Report{}, err
	}

	// Looked up the same way the worker looks it up, so this explains the
	// extraction that actually happens rather than a hypothetical one.
	rule, err := s.System().EffectiveRuleFor(ctx, reader, article.Host)
	if err != nil {
		return Report{}, fmt.Errorf("looking up the rules for %s: %w", article.Host, err)
	}

	feedBody, err := s.FeedBodyFor(ctx, id)
	if err != nil {
		return Report{}, fmt.Errorf("looking up the feed body: %w", err)
	}

	in := extract.Input{RawHTML: raw, URL: article.URLCanonical, FeedBody: feedBody}
	if rule.ContentSelector != "" || len(rule.StripSelectors) > 0 {
		in.Rule = &extract.Rule{
			ContentSelector: rule.ContentSelector,
			StripSelectors:  rule.StripSelectors,
		}
	}

	report := Report{Article: article, Rule: rule, RawBytes: len(raw)}
	report.Result, report.Steps, report.Err = extract.New().Explain(in)

	// The body on record for this reader — theirs if they have one, otherwise the
	// household's, which is what they are actually reading.
	owner := store.Household()
	if reader != store.HouseholdRule {
		if _, err := s.CurrentContent(ctx, id, store.Owned(reader)); err == nil {
			owner = store.Owned(reader)
		}
	}
	if stored, err := s.CurrentContent(ctx, id, owner); err == nil {
		report.Stored = stored
		report.HasStored = true
		// Stale in the sense that re-extracting would change it: either the program
		// has moved on or the rules have. An immutable body is never stale, because
		// nothing regenerates it.
		report.StoredStale = !stored.Immutable &&
			(stored.ExtractorVersion != extract.Version || stored.RulesetKey != rule.RulesetKey())
	}

	return report, nil
}

// storedPage loads and decompresses the page the ladder runs over.
func storedPage(ctx context.Context, blobs blob.Store, article store.Article) ([]byte, error) {
	if article.RawBlobPath == "" {
		// Not an error: an article can legitimately have no stored page, and what
		// this should then explain is a ladder with only the feed-body rung
		// available to it.
		return nil, nil
	}

	r, err := blobs.Get(ctx, article.RawBlobPath)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, fmt.Errorf("the stored page %s is missing from the archive; "+
				"the row points at a file that is not there", article.RawBlobPath)
		}
		return nil, fmt.Errorf("reading the stored page: %w", err)
	}
	defer func() { _ = r.Close() }()

	raw, err := decompress(r)
	if err != nil {
		return nil, fmt.Errorf("reading the stored page: %w", err)
	}
	return raw, nil
}

// decompress reads a gzipped stored page.
func decompress(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("decompressing: %w", err)
	}
	defer func() { _ = gz.Close() }()
	return io.ReadAll(gz)
}
