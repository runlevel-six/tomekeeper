package jobs

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"golang.org/x/sync/singleflight"

	"github.com/runlevel-six/tomekeeper/internal/archive"
	"github.com/runlevel-six/tomekeeper/internal/asset"
	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/exchange"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// LocalizeAssetsArgs asks for one article's images to be brought into the
// archive and its files written.
//
// Named for the article rather than the image, deliberately. It is one job
// per *article* rather than per image because the last step — rewriting the
// body so its references point at the archive — can only happen once every
// image has been resolved. Splitting per image would need a fan-in step and a
// way to know when the last one landed, which is more machinery for no gain at
// the handful of images an article carries.
type LocalizeAssetsArgs struct {
	ArticleID int64 `json:"article_id"`
}

// Kind implements river.JobArgs.
func (LocalizeAssetsArgs) Kind() string { return "localize_assets" }

// InsertOpts makes localization unique per article while one is outstanding.
func (LocalizeAssetsArgs) InsertOpts() river.InsertOpts {
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

// LocalizeAssetsWorker downloads an article's images, stores them under
// content addresses, rewrites the body to point at them, and writes the
// article's files.
//
// This is where "files are the archive" becomes literally true. After this job, the
// article is a directory containing a page that opens in a browser with this
// service stopped, the database gone, and the machine offline.
type LocalizeAssetsWorker struct {
	river.WorkerDefaults[LocalizeAssetsArgs]

	store   *store.Store
	client  *httpclient.Client
	blobs   blob.Store
	archive *archive.Writer
	log     *slog.Logger

	// inflight collapses concurrent work on the same source URL.
	//
	// The database check in resolve is a check-then-act: two articles sharing an
	// image, landing on two workers at the same moment, both miss it and both
	// fetch. The origin then serves one request per worker slot for a picture
	// the archive needs once, which is the impoliteness the politeness rules
	// exist to prevent. It is also what the "same image across ten articles"
	// criterion measures.
	//
	// Per-process, deliberately. Two worker replicas would still duplicate
	// across the concurrent window; closing that needs an advisory lock per
	// image, which is a round trip per reference to save at most one fetch while
	// the worker runs as a single Deployment. Revisit if the worker is ever
	// scaled out.
	inflight singleflight.Group
}

// Work implements river.Worker.
func (w *LocalizeAssetsWorker) Work(ctx context.Context, job *river.Job[LocalizeAssetsArgs]) error {
	id := store.ArticleID(job.Args.ArticleID)

	article, err := w.store.GetArticle(ctx, id)
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return err
	}

	content, err := w.store.CurrentContent(ctx, id)
	if err != nil {
		if store.IsNotFound(err) {
			// Nothing extracted, so there is no body to localize. Extraction
			// enqueues this job, so reaching here means the body was removed
			// in between; there is nothing to retry.
			return nil
		}
		return err
	}

	log := w.log.With("article_id", job.Args.ArticleID, "url", article.URLCanonical)

	localized, outcome := asset.Localize(content.HTML, w.resolver(ctx, id, article.URLCanonical, log))

	if outcome.Found > 0 {
		if err := w.store.UpdateContentHTML(ctx, id, localized); err != nil {
			return err
		}
	}

	status := store.AssetsOK
	switch {
	case outcome.Found == 0:
		status = store.AssetsNone
	case outcome.Failed > 0:
		// The asset policy: asset failures are non-fatal. The article keeps the absolute
		// URLs it could not localize, and is marked so the gap is visible
		// rather than being discovered years later as a broken image.
		status = store.AssetsPartial
	}
	if err := w.store.SetAssetsStatus(ctx, id, status); err != nil {
		return err
	}

	if err := w.writeFiles(ctx, article, content, localized); err != nil {
		return err
	}

	log.Info("localized assets",
		"found", outcome.Found,
		"localized", outcome.Localized,
		"failed", outcome.Failed,
		"status", status)
	return nil
}

// resolver returns the function Localize calls for each image reference.
//
// It fetches, processes, stores, and records — everything except deciding
// which references exist, which is Localize's job.
func (w *LocalizeAssetsWorker) resolver(
	ctx context.Context, id store.ArticleID, articleURL string, log *slog.Logger,
) asset.Resolver {
	// Within one article the same image often appears several times. Caching
	// here keeps the resolver from asking the origin twice for a picture it
	// has already stored a moment ago.
	seen := make(map[string]string)

	return func(sourceURL string) (string, bool) {
		if path, ok := seen[sourceURL]; ok {
			return path, path != ""
		}

		path, err := w.localize(ctx, id, articleURL, sourceURL)
		if err != nil {
			if reason, skipped := asset.IsSkipped(err); skipped {
				// A deliberate skip is not a failure. A tracking pixel left as
				// a hotlink costs nothing, and recording it as a failure would
				// mark half the archive 'partial'.
				log.Debug("image not localized", "url", sourceURL, "reason", reason)
				seen[sourceURL] = ""
				return "", true
			}
			log.Warn("image could not be localized", "url", sourceURL, "error", err)
			seen[sourceURL] = ""
			return "", false
		}

		seen[sourceURL] = path
		return path, true
	}
}

// localize resolves one image and links it to this article, returning its
// store-relative path.
func (w *LocalizeAssetsWorker) localize(
	ctx context.Context, id store.ArticleID, articleURL, sourceURL string,
) (string, error) {
	stored, err := w.resolve(ctx, articleURL, sourceURL)
	if err != nil {
		return "", err
	}

	// Linking stays outside the single-flight below: ten articles sharing one
	// image need one fetch and ten links, so this is the part that must run once
	// per caller rather than once per URL.
	if err := w.store.LinkAsset(ctx, id, stored.SHA256); err != nil {
		return "", err
	}
	return stored.FSPath, nil
}

// resolve returns the archived asset for sourceURL, fetching it only if it is
// not already stored and no other job is fetching it at this moment.
func (w *LocalizeAssetsWorker) resolve(
	ctx context.Context, articleURL, sourceURL string,
) (store.Asset, error) {
	v, err, _ := w.inflight.Do(sourceURL, func() (any, error) {
		// Already fetched from this URL, possibly for a different article. This
		// is the ten-articles-one-image case: the file is on disk and the row
		// exists, so all that is needed is the link the caller adds.
		if existing, err := w.store.AssetBySourceURL(ctx, sourceURL); err == nil {
			return existing, nil
		} else if !store.IsNotFound(err) {
			return store.Asset{}, err
		}
		return w.download(ctx, articleURL, sourceURL)
	})
	if err != nil {
		return store.Asset{}, err
	}

	stored, ok := v.(store.Asset)
	if !ok {
		return store.Asset{}, fmt.Errorf("resolving image %s: unexpected result %T", sourceURL, v)
	}
	return stored, nil
}

// download fetches, transcodes, and stores one image that is not yet archived.
func (w *LocalizeAssetsWorker) download(
	ctx context.Context, articleURL, sourceURL string,
) (store.Asset, error) {
	raw, mediaType, err := w.fetch(ctx, articleURL, sourceURL)
	if err != nil {
		return store.Asset{}, err
	}

	processed, err := asset.Process(raw, mediaType)
	if err != nil {
		return store.Asset{}, err
	}

	// The blob is written before the row, so a crash between them leaves an
	// orphaned file rather than a row pointing at nothing. An extra file is
	// harmless; a dangling reference breaks the page.
	exists, err := w.blobs.Exists(ctx, processed.Path)
	if err != nil {
		return store.Asset{}, err
	}
	if !exists {
		if err := w.blobs.Put(ctx, processed.Path, bytes.NewReader(processed.Bytes)); err != nil {
			return store.Asset{}, fmt.Errorf("storing image %s: %w", sourceURL, err)
		}
	}

	stored := store.Asset{
		SHA256:    processed.SHA256,
		MediaType: processed.MediaType,
		ByteSize:  int64(len(processed.Bytes)),
		Width:     processed.Width,
		Height:    processed.Height,
		FSPath:    processed.Path,
		SourceURL: sourceURL,
	}
	if _, err := w.store.UpsertAsset(ctx, stored); err != nil {
		return store.Asset{}, err
	}
	return stored, nil
}

// fetch downloads one image through the shared, rate-limited client.
func (w *LocalizeAssetsWorker) fetch(ctx context.Context, articleURL, sourceURL string) ([]byte, string, error) {
	header := make(http.Header, 1)
	// The asset policy: send the article as Referer. Many hosts serve images only to
	// requests that look like they came from the page, and this is honest —
	// the request genuinely was made on behalf of that page.
	header.Set("Referer", articleURL)

	resp, err := w.client.Do(ctx, httpclient.Request{URL: sourceURL, Header: header})
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	raw, err := httpclient.ReadBody(resp.Body)
	if err != nil {
		return nil, "", err
	}

	mediaType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	return raw, strings.TrimSpace(mediaType), nil
}

// writeFiles generates index.html and meta.json for the article.
func (w *LocalizeAssetsWorker) writeFiles(
	ctx context.Context, article store.Article, content store.Content, body string,
) error {
	assets, err := w.store.AssetsForArticle(ctx, article.ID)
	if err != nil {
		return err
	}

	records := make([]exchange.Asset, 0, len(assets))
	for _, a := range assets {
		records = append(records, exchange.Asset{
			Path:      a.FSPath,
			SourceURL: a.SourceURL,
			SHA256:    a.SHA256,
			MediaType: a.MediaType,
			ByteSize:  a.ByteSize,
			Width:     a.Width,
			Height:    a.Height,
		})
	}

	return w.archive.Write(ctx, archive.Article{
		Dir:              blob.ArticleDir(article.FirstSeenAt, article.Title, article.URLCanonical),
		URL:              article.URLCanonical,
		Title:            article.Title,
		Author:           article.Author,
		SiteName:         article.SiteName,
		Language:         article.Language,
		PublishedAt:      article.PublishedAt,
		ArchivedAt:       article.FirstSeenAt,
		ContentHTML:      body,
		WordCount:        content.WordCount,
		Extractor:        content.ExtractorName,
		ExtractorVersion: content.ExtractorVersion,
		Immutable:        content.Immutable,
		HasRaw:           article.RawBlobPath != "",
		Assets:           records,
	})
}

// ScheduleAssetsArgs asks for articles awaiting localization to be enqueued.
type ScheduleAssetsArgs struct{}

// Kind implements river.JobArgs.
func (ScheduleAssetsArgs) Kind() string { return "schedule_assets" }

// InsertOpts keeps overlapping scheduler runs from piling up.
func (ScheduleAssetsArgs) InsertOpts() river.InsertOpts {
	return river.InsertOpts{
		UniqueOpts: river.UniqueOpts{
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

// ScheduleAssetsWorker enqueues localization for articles still pending.
//
// Like the fetch scheduler, this finds nothing in steady state: extraction
// enqueues the job directly. It covers the backlog from before the asset
// pipeline existed and
// anything lost to a crash.
type ScheduleAssetsWorker struct {
	river.WorkerDefaults[ScheduleAssetsArgs]

	store  *store.Store
	client *river.Client[pgx.Tx]
	log    *slog.Logger
}

// Work implements river.Worker.
func (w *ScheduleAssetsWorker) Work(ctx context.Context, _ *river.Job[ScheduleAssetsArgs]) error {
	pending, err := w.store.System().PendingAssets(ctx, scheduleBatchSize)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	params := make([]river.InsertManyParams, 0, len(pending))
	for _, id := range pending {
		params = append(params, river.InsertManyParams{
			Args: LocalizeAssetsArgs{ArticleID: int64(id)},
		})
	}

	if _, err := w.client.InsertMany(ctx, params); err != nil {
		return fmt.Errorf("enqueueing %d asset jobs: %w", len(params), err)
	}

	w.log.Debug("scheduled asset localization", "articles", len(pending))
	return nil
}

// EnqueueLocalization queues one article's images for localization.
func EnqueueLocalization(ctx context.Context, client *river.Client[pgx.Tx], id store.ArticleID) error {
	if _, err := client.Insert(ctx, LocalizeAssetsArgs{ArticleID: int64(id)}, nil); err != nil {
		return fmt.Errorf("queueing asset localization for article %d: %w", id, err)
	}
	return nil
}
