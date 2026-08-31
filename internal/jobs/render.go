package jobs

import (
	"bytes"
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
	"github.com/runlevel-six/tomekeeper/internal/render"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// RenderQueue is the queue rendering runs on, apart from everything else.
//
// Its own queue for one reason: a browser that stops answering must cost the archive
// its renders and nothing more. The default queue carries polling, fetching,
// extraction and image localization at MaxWorkers slots, and a render that hangs for
// its full timeout in that pool holds a slot the whole time — enough of them and feeds
// stop being polled because one site's JavaScript will not finish. Here the blast
// radius is this queue's own width, which is deliberately one or two.
const RenderQueue = "render"

// RenderArticleArgs asks for one article's page to be fetched through a browser.
//
// A job of its own rather than a branch inside fetch_article, so that the queue an
// article is waiting in says what is happening to it, and so that the narrow
// concurrency above applies to renders alone.
type RenderArticleArgs struct {
	ArticleID int64 `json:"article_id"`
}

// Kind implements river.JobArgs.
func (RenderArticleArgs) Kind() string { return "render_article" }

// InsertOpts puts this on the render queue and makes it unique per article while one
// is outstanding, the same guard fetch_article carries: three feeds carrying one story
// must not each drive a browser.
func (RenderArticleArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		Queue: RenderQueue,
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

// RenderArticleWorker fetches a page through a headless browser and stores the
// rendered DOM as the article's raw page.
//
// What it stores is the DOM after the page's scripts have run — not the shell the
// server sent. That is the whole point: extraction then runs over it offline like any
// other page, `tome reextract` improves it later without touching the network, and the
// standalone copy in the archive holds an article rather than an empty div. See
// internal/render for why this is a fetch rather than a rung on the extraction ladder.
type RenderArticleWorker struct {
	river.WorkerDefaults[RenderArticleArgs]

	store    *store.Store
	renderer *render.Renderer
	client   *httpclient.Client
	blobs    blob.Store
	log      *slog.Logger
}

// Work implements river.Worker.
func (w *RenderArticleWorker) Work(ctx context.Context, job *river.Job[RenderArticleArgs]) error {
	id := store.ArticleID(job.Args.ArticleID)

	article, err := w.store.GetArticle(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}

	log := w.log.With("article_id", job.Args.ArticleID, "url", article.URLCanonical)

	if article.FetchStatus == store.FetchOK && article.RawBlobPath != "" {
		log.Debug("article already fetched, skipping the render")
		return nil
	}

	// Robots and the host's rate limit, through the same client and cache the ordinary
	// fetch path uses. A browser bypasses both unless somebody asks first, and asking
	// here rather than inside the render package keeps one implementation of the rule.
	if err := w.client.Permit(ctx, article.URLCanonical); err != nil {
		if errors.Is(err, httpclient.ErrDisallowedByRobots) && !interrupted(ctx) {
			log.Info("article disallowed by robots.txt, so it is not rendered either")
			return recordFetchFailure(ctx, w.store, id, store.FetchSkipped,
				"disallowed by robots.txt", log)
		}
		return err
	}

	page, err := w.renderer.Render(ctx, article.URLCanonical)
	if err != nil {
		// No browser is an operator's condition, not the article's, and the two must not
		// look the same afterwards. Returned for retry with the article left pending, so
		// that scaling the render deployment up later picks these up rather than leaving
		// a queue of articles marked failed for a reason that has since gone away.
		if errors.Is(err, render.ErrUnavailable) {
			// Pending, with the reason written down. Not `failed`: this is an operator's
			// deployment scaled to zero or a pod that died, and blaming the site for that
			// would also make it permanent, since a recorded failure is never retried.
			//
			// The note is what makes the wait visible. Without it the article sat pending
			// forever — retried every minute, failing every time — while the failed-fetch
			// queue ignored it and the reading list badged it "queued". A reader had no
			// way to find out, and neither did the operator who could have fixed it.
			const waiting = "waiting for a headless browser; none is reachable"
			recCtx, cancel := recording(ctx)
			defer cancel()
			if noteErr := w.store.RecordFetchWaiting(recCtx, id, waiting); noteErr != nil {
				log.Warn("could not record that this article is waiting", "error", noteErr)
			}
			log.Warn("no browser is available, so this article stays pending", "error", err)
			return err
		}
		if interrupted(ctx) {
			return err
		}
		// The page's own failure. Recorded, so it lands in the attention queue where a
		// site that needs a rule belongs.
		log.Warn("render failed", "error", err)
		return recordFetchFailure(ctx, w.store, id, store.FetchFailed,
			"rendering: "+describe(err, "the render ran out of time"), log)
	}

	raw := []byte(page.HTML)
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])

	path := blob.RawPath(article.FirstSeenAt, article.Title, article.URLCanonical)
	compressed, err := gzipBytes(raw)
	if err != nil {
		return err
	}
	if err := w.blobs.Put(ctx, path, bytes.NewReader(compressed)); err != nil {
		return fmt.Errorf("storing the rendered page: %w", err)
	}

	if err := w.store.RecordFetchSuccess(ctx, id, store.FetchedPage{
		SHA: sha, Path: path, BrowserRendered: true,
	}); err != nil {
		return err
	}

	// The two request counts are logged because they are the politeness numbers: a
	// render that allowed thirty subresources made thirty requests to hosts nobody
	// chose, and that is worth being able to see per article rather than inferring.
	log.Info("rendered article",
		"bytes", len(raw), "stored_bytes", len(compressed), "path", path,
		"subresources_blocked", page.Blocked, "subresources_allowed", page.Requests)

	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return fmt.Errorf("no river client in context; cannot enqueue extraction: %w", err)
	}
	if _, err := client.Insert(ctx, ExtractArticleArgs{ArticleID: job.Args.ArticleID}, nil); err != nil {
		return fmt.Errorf("enqueueing extraction: %w", err)
	}
	return nil
}
