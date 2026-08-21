package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// ExtractArticleArgs asks for one article's body to be extracted from its
// stored raw page.
type ExtractArticleArgs struct {
	ArticleID int64 `json:"article_id"`

	// Force re-runs extraction even when a current body already exists at the
	// current extractor version. `tome reextract` sets it; the pipeline does
	// not, so a duplicated job is cheap rather than wasteful.
	Force bool `json:"force,omitempty"`
}

// Kind implements river.JobArgs.
func (ExtractArticleArgs) Kind() string { return "extract_article" }

// InsertOpts makes extraction unique per article while one is outstanding.
func (ExtractArticleArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		UniqueOpts: river.UniqueOpts{
			ByArgs: true,
			ByState: []rivertype.JobState{
				rivertype.JobStateAvailable,
				rivertype.JobStatePending,
				rivertype.JobStateRunning,
				rivertype.JobStateRetryable,
				rivertype.JobStateScheduled,
			},
		},
	}
}

// ExtractArticleWorker runs the extraction ladder over a stored page.
//
// It never touches the network. Everything it needs — the raw page, the domain
// rule, the feed's own body — is already in the archive, which is what makes
// `tome reextract` able to reprocess ten years of articles without asking a
// single server for anything.
type ExtractArticleWorker struct {
	river.WorkerDefaults[ExtractArticleArgs]

	store     *store.Store
	blobs     blob.Store
	extractor *extract.Extractor
	log       *slog.Logger
}

// Work implements river.Worker.
func (w *ExtractArticleWorker) Work(ctx context.Context, job *river.Job[ExtractArticleArgs]) error {
	id := store.ArticleID(job.Args.ArticleID)

	article, err := w.store.GetArticle(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}

	log := w.log.With("article_id", job.Args.ArticleID, "url", article.URLCanonical)

	if !job.Args.Force && w.alreadyCurrent(ctx, id) {
		log.Debug("article already extracted at the current version, skipping")
		return nil
	}

	raw, err := w.readRaw(ctx, article)
	if err != nil {
		// The page is gone from the blob store but the database says it is
		// there. That is a real inconsistency worth retrying and, if it
		// persists, worth someone looking at.
		return err
	}

	rule, err := w.ruleFor(ctx, article.URLCanonical)
	if err != nil {
		return err
	}

	feedBody, err := w.store.FeedBodyFor(ctx, id)
	if err != nil {
		return err
	}

	result, err := w.extractor.Extract(extract.Input{
		RawHTML:  raw,
		URL:      article.URLCanonical,
		Rule:     rule,
		FeedBody: feedBody,
	})
	// Recorded before the error is handled, because the failing case is the one that
	// needs both of these: an article with no body is what somebody is looking at when
	// they ask whether this site wants a browser or a selector, and it is the article
	// `tome reextract` could not previously find at all. A version recorded only for
	// successes is a version that cannot describe a failure.
	if err := w.store.RecordExtractAttempt(ctx, id, extract.Version, result.PageVisibleChars); err != nil {
		// Not fatal to the extraction. These are diagnostics, and losing them must not
		// cost the body that was just produced.
		log.Warn("could not record the extraction attempt", "error", err)
	}

	if err != nil {
		if errors.Is(err, extract.ErrNoContent) {
			// Expected, not exceptional: paywalls, JavaScript shells, and
			// pages that 200 while saying nothing. Recording it puts the
			// article in the queue that domain rules exist to drain, instead
			// of retrying a page that will not change.
			//
			// The two cases are worth telling apart in the queue, because they
			// call for different things. "The extractors found nothing" is a
			// page that needs a domain rule; "there was nothing to extract from"
			// is an article whose fetch never landed, which is a different
			// problem entirely and reads as the first one if both say the same
			// sentence. Discovered the hard way: a stray job extracted an
			// article that had not been fetched, and the resulting "extraction
			// produced no content" sent the investigation a long way from the
			// cause.
			reason := "extraction produced no content"
			if len(raw) == 0 && feedBody == "" {
				reason = "no stored page and no feed body to extract from"
			}

			log.Info("no extractor produced acceptable content", "reason", reason)
			return w.store.RecordFetchFailure(ctx, id, store.FetchFailed, reason)
		}
		return err
	}

	origin := store.OriginFetched
	if result.Name == extract.NameFeedBody {
		origin = store.OriginFeedBody
	}

	madeCurrent, err := w.store.InsertContent(ctx, store.ContentParams{
		ArticleID:        id,
		ExtractorName:    result.Name,
		ExtractorVersion: extract.Version,
		ContentOrigin:    origin,
		HTML:             result.HTML,
		Text:             result.Text,
		WordCount:        result.WordCount,
	})
	if err != nil {
		return err
	}

	// The page usually knows more about itself than the feed did. Gaps only:
	// whatever the feed supplied has already been seen and is not churned.
	if err := w.store.UpdateArticleMetadata(ctx, id, store.ArticleParams{
		Title:       result.Title,
		Author:      result.Author,
		SiteName:    result.SiteName,
		Language:    result.Language,
		PublishedAt: result.PublishedAt,
	}); err != nil {
		return err
	}

	log.Info("extracted article",
		"extractor", result.Name,
		"words", result.WordCount,
		"characters", len(result.Text),
		"is_current", madeCurrent)

	if !madeCurrent {
		// The imported-content-is-immutable principle: an imported body may be the only surviving copy of a dead URL,
		// so a fetched body is stored beside it rather than over it. Promoting
		// one over the other is a deliberate human act.
		log.Info("stored alongside an immutable body rather than replacing it")
		// The current body is unchanged, so its images are already localized
		// and its files already written.
		return nil
	}

	// This article now has a body, so any failure recorded when it did not is
	// history rather than state. Done here rather than inside InsertContent
	// because a body stored beside an immutable one changes nothing about the
	// article's standing — the early return above has already left.
	if err := w.store.ClearExtractionFailure(ctx, id); err != nil {
		return err
	}

	// Localizing images and writing the article's files is a separate job:
	// downloading a dozen images is slow and impolite to repeat, and it must
	// not be able to cost the extraction that just succeeded.
	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		return fmt.Errorf("no river client in context; cannot enqueue asset localization")
	}
	return EnqueueLocalization(ctx, client, id)
}

// alreadyCurrent reports whether this article already has a body from the
// current extractor version.
func (w *ExtractArticleWorker) alreadyCurrent(ctx context.Context, id store.ArticleID) bool {
	current, err := w.store.CurrentContent(ctx, id)
	if err != nil {
		return false
	}
	return current.ExtractorVersion == extract.Version
}

// readRaw loads and decompresses the stored page.
func (w *ExtractArticleWorker) readRaw(ctx context.Context, article store.Article) ([]byte, error) {
	if article.RawBlobPath == "" {
		// Nothing was fetched. The ladder can still fall back to the feed
		// body, which is exactly the case that rung exists for.
		return nil, nil
	}

	r, err := w.blobs.Get(ctx, article.RawBlobPath)
	if err != nil {
		if errors.Is(err, blob.ErrNotFound) {
			return nil, fmt.Errorf("the stored page %s is missing from the blob store: %w",
				article.RawBlobPath, err)
		}
		return nil, err
	}
	defer func() { _ = r.Close() }()

	compressed, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", article.RawBlobPath, err)
	}
	return gunzipBytes(bytes.NewReader(compressed))
}

// ruleFor finds the domain rule covering an article's host, if any.
func (w *ExtractArticleWorker) ruleFor(ctx context.Context, articleURL string) (*extract.Rule, error) {
	u, err := url.Parse(articleURL)
	if err != nil {
		return nil, nil //nolint:nilerr // an unparseable URL simply has no rule
	}

	rule, err := w.store.System().DomainRuleFor(ctx, u.Hostname())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	return &extract.Rule{
		ContentSelector: rule.ContentSelector,
		StripSelectors:  rule.StripSelectors,
	}, nil
}
