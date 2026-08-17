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

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
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
// Storing the raw fetch is principle 2.2: extraction quality only improves, so
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
		// need to serve, and principle 2.3 says a re-fetch is a new version
		// rather than an overwrite — a decision for `tome reextract`, not for
		// a duplicate job.
		log.Debug("article already fetched, skipping")
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

	if err := w.store.RecordFetchSuccess(ctx, id, sha, path); err != nil {
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
