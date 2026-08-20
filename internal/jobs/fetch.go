package jobs

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/render"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// FetchArticleArgs asks for one article's page to be fetched and stored.
//
// No UserID: articles are a global pool, and the page is the same page
// whichever subscription led to it.
type FetchArticleArgs struct {
	ArticleID int64 `json:"article_id"`
}

// Kind implements river.JobArgs.
func (FetchArticleArgs) Kind() string { return "fetch_article" }

// InsertOpts makes a fetch unique per article while one is outstanding, so
// that three feeds carrying the same story do not each fetch the page.
func (FetchArticleArgs) InsertOpts() river.InsertOpts {
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

// FetchArticleWorker downloads an article page and stores the raw bytes.
//
// The raw fetch is stored because extraction quality only improves, so
// a body is a derived view that can be regenerated. If the page were not kept,
// every future extraction improvement would apply only to articles fetched
// after it, and the decade of archive behind it would stay as bad as the day
// it was saved.
type FetchArticleWorker struct {
	river.WorkerDefaults[FetchArticleArgs]

	store  *store.Store
	client *httpclient.Client
	blobs  blob.Store
	log    *slog.Logger

	// renderer is nil unless a browser is configured, which is the ordinary case.
	// Its only use here is deciding whether a flagged domain can be handed off.
	renderer *render.Renderer
}

// Work implements river.Worker.
func (w *FetchArticleWorker) Work(ctx context.Context, job *river.Job[FetchArticleArgs]) error {
	id := store.ArticleID(job.Args.ArticleID)

	article, err := w.store.GetArticle(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Deleted between enqueue and execution. Nothing to do, and
			// nothing worth retrying.
			return nil
		}
		return err
	}

	log := w.log.With("article_id", job.Args.ArticleID, "url", article.URLCanonical)

	if article.FetchStatus == store.FetchOK && article.RawBlobPath != "" {
		// Already fetched. Re-fetching would be a request the origin did not
		// need to serve, and a re-fetch is a new version
		// rather than an overwrite — a decision for `tome reextract`, not for
		// a duplicate job.
		log.Debug("article already fetched, skipping")
		return nil
	}

	// A domain somebody has flagged as needing JavaScript is handed to the render
	// queue instead of being fetched here.
	//
	// Checked at fetch time rather than at enqueue time deliberately: the flag is data
	// an operator changes while the archive runs, and the article that motivated the
	// flag is usually already sitting in the queue when they set it. Deciding here means
	// the next attempt honors it; deciding at insert would have frozen the answer before
	// anybody knew it was wrong.
	rendered, err := w.handOffToBrowser(ctx, article, log)
	if err != nil {
		return err
	}
	if rendered {
		return nil
	}

	resp, err := w.client.Get(ctx, article.URLCanonical, nil)
	if err != nil {
		// A site saying no through robots.txt is not a failure. It will not
		// change on retry, and recording it as skipped keeps the failed-fetch
		// queue meaningful.
		if errors.Is(err, httpclient.ErrDisallowedByRobots) {
			log.Info("article disallowed by robots.txt")
			return w.store.RecordFetchFailure(ctx, id, store.FetchSkipped, "disallowed by robots.txt")
		}
		log.Warn("fetch failed", "error", err)
		return w.store.RecordFetchFailure(ctx, id, store.FetchFailed, err.Error())
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		reason := fmt.Sprintf("HTTP %d", resp.StatusCode)
		log.Warn("fetch failed", "status", resp.StatusCode)
		return w.store.RecordFetchFailure(ctx, id, store.FetchFailed, reason)
	}

	raw, err := httpclient.ReadBody(resp.Body)
	if err != nil {
		log.Warn("reading the page failed", "error", err)
		return w.store.RecordFetchFailure(ctx, id, store.FetchFailed, err.Error())
	}

	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])

	path := blob.RawPath(article.FirstSeenAt, article.Title, article.URLCanonical)
	compressed, err := gzipBytes(raw)
	if err != nil {
		return err
	}

	// A storage failure is our problem, not the site's, so it is returned for
	// retry rather than written to the article as a fetch failure.
	if err := w.blobs.Put(ctx, path, bytes.NewReader(compressed)); err != nil {
		return fmt.Errorf("storing the raw page: %w", err)
	}

	if err := w.store.RecordFetchSuccess(ctx, id, store.FetchedPage{SHA: sha, Path: path}); err != nil {
		return err
	}

	log.Info("fetched article",
		"bytes", len(raw), "stored_bytes", len(compressed), "path", path)

	// Extraction is a separate job so that an extractor crash or a slow
	// extraction cannot cost the fetch, which is the expensive, impolite-to-
	// repeat half of the work.
	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		return fmt.Errorf("no river client in context; cannot enqueue extraction")
	}
	if _, err := client.Insert(ctx, ExtractArticleArgs{ArticleID: job.Args.ArticleID}, nil); err != nil {
		return fmt.Errorf("enqueueing extraction: %w", err)
	}
	return nil
}

// gzipBytes compresses the raw page.
//
// HTML compresses to roughly a fifth of its size, and the archive keeps every
// page it ever fetched. Over a decade that ratio is the difference between a
// disk that fills and one that does not.
func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer

	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, fmt.Errorf("compressing the page: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("finishing compression: %w", err)
	}
	return buf.Bytes(), nil
}

// gunzipBytes reverses gzipBytes.
func gunzipBytes(r *bytes.Reader) ([]byte, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("reading the compressed page: %w", err)
	}
	defer func() { _ = zr.Close() }()

	return httpclient.ReadBody(zr)
}

// handOffToBrowser enqueues a render for a flagged domain, reporting whether it did.
//
// Three conditions have to hold, and the order matters because each one's failure
// wants a different answer:
//
//   - The host has a domain rule with requires_js set. Rules are looked up by walking
//     up the domain, so a rule for example.com covers blog.example.com — the same
//     semantics `tome reextract --domain` and the rules page already use.
//   - A renderer is configured. If none is, the article is fetched plainly instead of
//     failing: a shell is a poor archive copy but it is better than nothing, it lands
//     in the attention queue where the operator will see it, and an installation that
//     flagged a domain and never deployed a browser has made a configuration mistake
//     rather than a request.
//   - The render queue can be reached. Enqueueing is the only thing that can fail here
//     and it is returned, because silently fetching the shell after deciding not to
//     would be the worst of both.
func (w *FetchArticleWorker) handOffToBrowser(
	ctx context.Context, article store.Article, log *slog.Logger,
) (bool, error) {
	if w.renderer == nil {
		return false, nil
	}

	host, err := hostOf(article.URLCanonical)
	if err != nil {
		// Not a reason to abandon the fetch: canonicalization produced this URL, so a
		// host that will not parse is a bug worth logging rather than a page to skip.
		log.Warn("could not read the host from a canonical URL", "error", err)
		return false, nil
	}

	rule, err := w.store.System().DomainRuleFor(ctx, host)
	switch {
	case store.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("looking up the domain rule for %s: %w", host, err)
	case !rule.RequiresJS:
		return false, nil
	}

	client := river.ClientFromContext[pgx.Tx](ctx)
	if client == nil {
		return false, fmt.Errorf("no river client in context; cannot enqueue a render")
	}
	if _, err := client.Insert(ctx, RenderArticleArgs{ArticleID: int64(article.ID)}, nil); err != nil {
		return false, fmt.Errorf("enqueueing a render: %w", err)
	}

	log.Info("handed to the render queue", "domain_rule", rule.Domain)
	return true, nil
}

// hostOf is the host a canonical URL names.
func hostOf(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("URL %q has no host", rawURL)
	}
	return u.Hostname(), nil
}
