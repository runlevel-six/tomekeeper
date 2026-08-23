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
	// Still gated on retention being configured somewhere, so that an archive with
	// it off does nothing here — but the window itself no longer decides anything.
	// Forgetting applies each reader's own, and this only asks whether every reader
	// has let go.
	if w.retain <= 0 {
		readers, err := w.store.ReadersWithRetention(ctx, 0)
		if err != nil {
			return err
		}
		if len(readers) == 0 {
			return nil
		}
	}

	candidates, err := w.store.ExpirableArticles(ctx, expireBatchSize)
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

// ForgetReadingArgs asks for readers' old reading to be forgotten.
type ForgetReadingArgs struct{}

// Kind implements river.JobArgs.
func (ForgetReadingArgs) Kind() string { return "forget_reading" }

// InsertOpts keeps overlapping runs from piling up, for the same reason expiry
// does: two of these would race on the same rows.
func (ForgetReadingArgs) InsertOpts() river.InsertOpts {
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

// ForgetReadingWorker lets each reader's old reading lapse on their own schedule.
//
// This runs before expiry means anything, and the ordering is worth stating: a
// reader forgetting an article releases their claim on it, and only when every
// claim has lapsed does ExpireContentWorker release the bytes. So this is the
// per-reader half — a privacy setting, which ages out what somebody read and when
// — and expiry is the household half, which reclaims disk once nobody minds.
//
// A reader with no window of their own follows the archive's. Zero on both means
// keep everything, and then this does nothing, which is the default.
type ForgetReadingWorker struct {
	river.WorkerDefaults[ForgetReadingArgs]

	store  *store.Store
	retain time.Duration
	log    *slog.Logger
}

// Work implements river.Worker.
func (w *ForgetReadingWorker) Work(ctx context.Context, _ *river.Job[ForgetReadingArgs]) error {
	readers, err := w.store.ReadersWithRetention(ctx, w.retain)
	if err != nil {
		return err
	}
	if len(readers) == 0 {
		return nil
	}

	// Per reader, each against their own cutoff. One batch each per run rather
	// than draining one reader before starting the next: this deletes things, and
	// a bug that ran away would otherwise take one person's entire history before
	// anybody else's first row.
	for userID, window := range readers {
		forgotten, err := w.store.ForgetReadArticles(ctx, userID, time.Now().Add(-window), expireBatchSize)
		if err != nil {
			// One reader's failure must not stop the others: a broken row in one
			// account is not a reason for everybody's retention to stop working.
			w.log.Error("forgetting old reading failed", "user_id", userID, "error", err)
			continue
		}
		if forgotten.Tombstoned == 0 && forgotten.Deleted == 0 {
			continue
		}

		w.log.Info("forgot old reading",
			"user_id", userID, "window", window,
			"forgotten", forgotten.Tombstoned, "removed", forgotten.Deleted)
	}
	return nil
}

// forgetPeriodicJob schedules the forgetting sweep.
func forgetPeriodicJob() *river.PeriodicJob {
	return river.NewPeriodicJob(
		river.PeriodicInterval(expiryInterval),
		func() (river.JobArgs, *river.InsertOpts) { return ForgetReadingArgs{}, nil },
		// Not RunOnStart, for the same reason expiry is not: this removes things,
		// and a worker whose first act after every restart is a deletion is one
		// whose crash loop is expensive.
		nil,
	)
}
