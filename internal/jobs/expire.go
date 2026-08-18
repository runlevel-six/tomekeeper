package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// ExpireContentArgs asks for expired article bodies to be released.
type ExpireContentArgs struct{}

// Kind implements river.JobArgs.
func (ExpireContentArgs) Kind() string { return "expire_content" }

// InsertOpts keeps overlapping expiry runs from piling up. Two of these racing
// would both try to delete the same blobs, and the loser would log failures for
// work the winner had already done correctly.
func (ExpireContentArgs) InsertOpts() river.InsertOpts {
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

// expireBatchSize bounds one run.
//
// Small on purpose. This deletes things, and a bug here is not recoverable by
// re-running; a bounded batch means the first bad run costs a hundred articles
// rather than the archive. It also keeps one run's blob deletions short enough
// that a worker restart mid-batch leaves little to reconcile.
const expireBatchSize = 100

// ExpireContentWorker releases the bodies and images of articles every reader
// has finished with.
//
// Does nothing at all unless retention is configured. The zero value means keep
// everything, and this worker is registered either way so that turning the
// setting on does not require knowing that a different set of jobs exists.
type ExpireContentWorker struct {
	river.WorkerDefaults[ExpireContentArgs]

	store  *store.Store
	blobs  blob.Store
	retain time.Duration
	log    *slog.Logger
}

// Work implements river.Worker.
func (w *ExpireContentWorker) Work(ctx context.Context, _ *river.Job[ExpireContentArgs]) error {
	if w.retain <= 0 {
		return nil
	}

	cutoff := time.Now().Add(-w.retain)

	candidates, err := w.store.ExpirableArticles(ctx, cutoff, expireBatchSize)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}

	var (
		expired    int
		bodyBytes  int64
		assetBytes int64
	)

	for _, c := range candidates {
		result, err := w.store.ExpireArticle(ctx, c.ArticleID)
		if err != nil {
			// One article failing must not stop the rest, for the same reason it
			// must not in an import: the failure is usually about that row.
			w.log.Warn("expiring an article failed",
				"article_id", c.ArticleID, "url", c.URL, "error", err)
			continue
		}

		// Files after the commit, never before. A blob deleted for a transaction
		// that then rolls back is gone for good, whereas a blob left behind by a
		// committed transaction is only wasted space — and the second failure is
		// the one that can be swept up later.
		w.removeBlobs(ctx, c, result)

		expired++
		bodyBytes += result.BodyBytes
		assetBytes += result.AssetBytes

		w.log.Debug("expired an article's content",
			"article_id", c.ArticleID, "title", c.Title, "url", c.URL,
			"body_bytes", result.BodyBytes, "asset_bytes", result.AssetBytes)
	}

	// At Info, because deleting a reader's archived pages should never be
	// something they have to turn on debug logging to find out about.
	w.log.Info("released expired content",
		"articles", expired, "body_bytes", bodyBytes, "asset_bytes", assetBytes,
		"retain_after_read", w.retain)

	if len(candidates) == expireBatchSize {
		w.log.Info("the expiry batch was full; more remains for the next run",
			"batch_size", expireBatchSize)
	}
	return nil
}

// removeBlobs deletes the files an expiry orphaned.
//
// Failures are logged and swallowed. The database has already committed, so
// returning an error here would retry an expiry that has nothing left to do and
// would report a permanent failure for an article that was expired correctly.
// The cost of a missed file is disk space; the cost of a retry loop is worse.
func (w *ExpireContentWorker) removeBlobs(ctx context.Context, c store.Expirable, result store.Expired) {
	paths := result.AssetPaths
	if result.RawPath != "" {
		paths = append(paths, result.RawPath)
	}

	for _, path := range paths {
		if err := w.blobs.Delete(ctx, path); err != nil && !errors.Is(err, blob.ErrNotFound) {
			w.log.Warn("deleting an expired blob failed; the row is gone but the file remains",
				"article_id", c.ArticleID, "path", path, "error", err)
		}
	}
}

var _ river.Worker[ExpireContentArgs] = (*ExpireContentWorker)(nil)

// expiryInterval is how often expiry runs.
//
// Hourly rather than on the schedule interval: retention is measured in days, so
// checking every minute would be a thousand pointless queries between anything
// actually becoming expirable.
const expiryInterval = time.Hour

func expiryPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(expiryInterval),
		func() (river.JobArgs, *river.InsertOpts) { return ExpireContentArgs{}, nil },
		// Deliberately not RunOnStart. The other schedulers catch up on work that
		// should already have happened; this one deletes things, and a worker that
		// starts by deleting is a worker whose first crash-loop restart is
		// expensive.
		nil,
	)
}
