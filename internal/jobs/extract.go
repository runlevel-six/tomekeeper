package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

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

	// UserID is the reader this extraction is for, or zero for the household's.
	//
	// Zero rather than a pointer because these are River args: a JSON field that is
	// absent and one that is null both have to mean the same thing, and no user has
	// id zero. It also keeps uniqueness working — UniqueOpts is ByArgs, so adding
	// this field makes an outstanding job unique per (article, reader) without
	// further configuration, which is what stops a rule change and the backstop
	// sweep from queueing the same work twice.
	UserID int64 `json:"user_id,omitempty"`
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

	// Whose extraction this is. The household's unless a reader asked, and the
	// distinction decides both which body slot is written and which of this job's
	// side effects are allowed to happen at all.
	reader := store.UserID(job.Args.UserID)
	owner := store.Household()
	if reader != store.HouseholdRule {
		owner = store.Owned(reader)
	}

	log := w.log.With("article_id", job.Args.ArticleID, "url", article.URLCanonical)
	if reader != store.HouseholdRule {
		log = log.With("user_id", job.Args.UserID)
	}

	// The rule is resolved before the skip check, because the skip depends on it:
	// a body is current only if it was produced by both this extractor version and
	// these rules.
	rule, err := w.store.System().EffectiveRuleFor(ctx, reader, article.Host)
	if err != nil {
		return err
	}
	rulesetKey := rule.RulesetKey()

	if !job.Args.Force && w.alreadyCurrent(ctx, id, owner, rulesetKey) {
		log.Debug("article already extracted at the current version and rules, skipping")
		return nil
	}

	raw, err := w.readRaw(ctx, article)
	if err != nil {
		// The page is gone from the blob store but the database says it is
		// there. That is a real inconsistency worth retrying and, if it
		// persists, worth someone looking at.
		return err
	}

	feedBody, err := w.store.FeedBodyFor(ctx, id)
	if err != nil {
		return err
	}

	result, err := w.extractor.Extract(extract.Input{
		RawHTML:  raw,
		URL:      article.URLCanonical,
		Rule:     extractRule(rule),
		FeedBody: feedBody,
	})
	// Recorded before the error is handled, because the failing case is the one that
	// needs both of these: an article with no body is what somebody is looking at when
	// they ask whether this site wants a browser or a selector, and it is the article
	// `tome reextract` could not previously find at all. A version recorded only for
	// successes is a version that cannot describe a failure.
	//
	// The household's runs only. page_visible_chars is a fact about the page and
	// would be the same either way, but extract_attempt_version is what the
	// household's re-extraction sweep reads to find bodyless articles — writing a
	// reader's attempt into it would tell that sweep the household had tried when
	// it had not, and the article would stop being a candidate.
	if reader == store.HouseholdRule {
		if err := w.store.RecordExtractAttempt(ctx, id, extract.Version, result.PageVisibleChars); err != nil {
			// Not fatal to the extraction. These are diagnostics, and losing them must not
			// cost the body that was just produced.
			log.Warn("could not record the extraction attempt", "error", err)
		}
	}

	if err != nil {
		if errors.Is(err, extract.ErrNoContent) {
			// An article that already has a body is not a failure. A bulk
			// reprocess runs the current ladder over every stored page, and a
			// page whose body was produced by older behavior may simply not
			// extract again — the reader still has the article, and there is
			// nothing here for anyone to fix.
			//
			// Recorded as a failure until 2026-08-21, when a version bump put
			// eight such articles into the attention queue in one run, every one
			// of them holding a perfectly good body. That is the same emptiness
			// ClearExtractionFailure was written for, arriving from the other
			// direction: a queue that lists work nobody can do is a queue that
			// stops being read.
			if current, err := w.store.CurrentContent(ctx, id, owner); err == nil && current.HTML != "" {
				log.Info("reprocessing produced nothing; keeping the existing body",
					"extractor", current.ExtractorName,
					"version", current.ExtractorVersion)
				return nil
			}

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

			// A reader's rules producing nothing is not the article's failure.
			//
			// fetch_status is article-level and shared, so recording it here would
			// put the page in *everybody's* attention queue because one reader wrote
			// a selector that matches nothing. They simply get no body of their own
			// and fall back to the household's, which is the right outcome and the
			// reason the fallback exists.
			//
			// What they do not get is a way to find out, and that is a real gap
			// rather than a decision: the remedy is the domain-rules page saying how
			// many of their articles a rule currently matches, which is interface
			// work rather than state to store.
			if reader != store.HouseholdRule {
				log.Info("the reader's rules produced no body; they keep the household's")
				return nil
			}

			if interrupted(ctx) {
				return ctx.Err()
			}
			recCtx, cancel := recording(ctx)
			defer cancel()
			return w.store.RecordFetchFailure(recCtx, id, store.FetchFailed, reason)
		}
		return err
	}

	origin := store.OriginFetched
	if result.Name == extract.NameFeedBody {
		origin = store.OriginFeedBody
	}

	madeCurrent, err := w.store.InsertContent(ctx, store.ContentParams{
		ArticleID:        id,
		Owner:            owner,
		ExtractorName:    result.Name,
		ExtractorVersion: extract.Version,
		ContentOrigin:    origin,
		RulesetKey:       rulesetKey,
		HTML:             result.HTML,
		Text:             result.Text,
		WordCount:        result.WordCount,
	})
	if err != nil {
		return err
	}

	// The page usually knows more about itself than the feed did. Gaps only:
	// whatever the feed supplied has already been seen and is not churned.
	//
	// The household's runs only, for the same reason as the attempt above: title,
	// author and language are article-level and shared, and a reader's selector may
	// well pick a different heading. One reader's rule must not rename an article
	// in everybody's list.
	if reader == store.HouseholdRule {
		if err := w.store.UpdateArticleMetadata(ctx, id, store.ArticleParams{
			Title:       result.Title,
			Author:      result.Author,
			SiteName:    result.SiteName,
			Language:    result.Language,
			PublishedAt: result.PublishedAt,
		}); err != nil {
			return err
		}
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
	//
	// The household's runs only. A reader's successful extraction says nothing
	// about whether the archive as a whole managed to extract this page, and
	// clearing the failure on their behalf would empty an attention queue entry
	// that is still true for everybody else.
	if reader == store.HouseholdRule {
		if err := w.store.ClearExtractionFailure(ctx, id); err != nil {
			return err
		}
	}

	// Localizing images and writing the article's files is a separate job:
	// downloading a dozen images is slow and impolite to repeat, and it must
	// not be able to cost the extraction that just succeeded.
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return fmt.Errorf("no river client in context; cannot enqueue asset localization: %w", err)
	}
	return EnqueueLocalization(ctx, client, id, reader)
}

// alreadyCurrent reports whether this owner's slot already holds a body produced
// by the current extractor version *and* the current rules.
//
// Both halves, because either can move independently: the program improves, or the
// reader edits a selector. Checking only the version is what let a rule change
// leave every affected body in place while reporting success.
func (w *ExtractArticleWorker) alreadyCurrent(
	ctx context.Context, id store.ArticleID, owner *store.UserID, rulesetKey string,
) bool {
	current, err := w.store.CurrentContent(ctx, id, owner)
	if err != nil {
		return false
	}
	return current.ExtractorVersion == extract.Version && current.RulesetKey == rulesetKey
}

// extractRule converts a resolved rule into what the extractor takes, or nil when
// no rule applied.
//
// Only the extraction half crosses over: the extractor works from a page already
// on disk and has no opinion about how it got there.
func extractRule(r store.EffectiveRule) *extract.Rule {
	if r.ContentSelector == "" && len(r.StripSelectors) == 0 {
		return nil
	}
	return &extract.Rule{
		ContentSelector: r.ContentSelector,
		StripSelectors:  r.StripSelectors,
	}
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
