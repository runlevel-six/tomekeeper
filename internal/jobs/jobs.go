// Package jobs defines the background work River performs and wires the
// client that runs it.
//
// `schedule_feeds` runs on a timer and asks
// which feeds are due; `poll_feed` polls one of them. The split matters: the
// scheduler is one cheap query, while a poll is a network round trip that may
// take thirty seconds, and putting both in one job would serialize every feed
// behind the slowest server in the list.
//
// fetch_article, extract_article, and localize_assets run on the same client.
package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// ScheduleInterval is how often the scheduler looks for feeds that are due.
//
// Feed intervals have a 15-minute floor, so checking every minute is ample
// resolution while costing one indexed query a minute.
const ScheduleInterval = time.Minute

// scheduleBatchSize bounds how many feeds one scheduler run will enqueue.
//
// The bound exists so that a first import of several hundred feeds — all due
// immediately, since next_poll_at defaults to now() — is spread over several
// minutes instead of arriving as one burst. Nothing is lost: whatever is not
// enqueued this minute is still due next minute.
const scheduleBatchSize = 100

// PollFeedArgs asks for one feed to be polled.
//
// UserID travels with FeedID even though a feed belongs to exactly one user
// and the id would be enough to find it. Carrying it means the worker calls
// user-scoped store methods rather than looking the owner up and trusting what
// it finds, so the scoping discipline holds through the queue as well as through the database.
type PollFeedArgs struct {
	UserID int64 `json:"user_id"`
	FeedID int64 `json:"feed_id"`
}

// Kind implements river.JobArgs.
func (PollFeedArgs) Kind() string { return "poll_feed" }

// InsertOpts makes a poll unique per feed while one is pending or running.
//
// Without this, a scheduler run that overlaps a slow poll would enqueue a
// second poll of the same feed, and the two would race to write the same
// conditional-GET validators. The feed's own server would also, quite
// reasonably, see it as hammering.
func (PollFeedArgs) InsertOpts() river.InsertOpts {
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

// ScheduleFeedsArgs asks for due feeds to be enqueued.
type ScheduleFeedsArgs struct{}

// Kind implements river.JobArgs.
func (ScheduleFeedsArgs) Kind() string { return "schedule_feeds" }

// PollFeedWorker polls a single feed.
type PollFeedWorker struct {
	river.WorkerDefaults[PollFeedArgs]

	poller *feed.Poller
}

// Work implements river.Worker.
func (w *PollFeedWorker) Work(ctx context.Context, job *river.Job[PollFeedArgs]) error {
	result, err := w.poller.Poll(ctx, store.UserID(job.Args.UserID), store.FeedID(job.Args.FeedID))
	if err != nil {
		// The poller records a feed's own failures against the feed and
		// returns nil, so anything returned here is our fault and worth a
		// retry.
		return err
	}
	if len(result.NewArticleIDs) == 0 {
		return nil
	}

	// Fetching is a separate job per article: one slow origin must not hold up
	// the rest of the feed, and a fetch that fails should retry on its own
	// schedule rather than re-polling the feed.
	client, err := river.ClientFromContextSafely[pgx.Tx](ctx)
	if err != nil {
		return fmt.Errorf("no river client in context; cannot enqueue fetches: %w", err)
	}

	params := make([]river.InsertManyParams, 0, len(result.NewArticleIDs))
	for _, id := range result.NewArticleIDs {
		params = append(params, river.InsertManyParams{
			Args: FetchArticleArgs{ArticleID: int64(id)},
		})
	}
	if _, err := client.InsertMany(ctx, params); err != nil {
		return fmt.Errorf("enqueueing %d article fetches: %w", len(params), err)
	}
	return nil
}

// ScheduleFeedsWorker enqueues a poll for every feed whose time has come.
type ScheduleFeedsWorker struct {
	river.WorkerDefaults[ScheduleFeedsArgs]

	store  *store.Store
	client *river.Client[pgx.Tx]
	log    *slog.Logger
}

// Work implements river.Worker.
func (w *ScheduleFeedsWorker) Work(ctx context.Context, _ *river.Job[ScheduleFeedsArgs]) error {
	// System scope: the scheduler serves every user at once. Each result
	// carries its owning user so the work it produces is scoped again.
	due, err := w.store.System().DueFeeds(ctx, scheduleBatchSize)
	if err != nil {
		return err
	}
	if len(due) == 0 {
		return nil
	}

	params := make([]river.InsertManyParams, 0, len(due))
	for _, d := range due {
		params = append(params, river.InsertManyParams{
			Args: PollFeedArgs{UserID: int64(d.UserID), FeedID: int64(d.FeedID)},
		})
	}

	results, err := w.client.InsertMany(ctx, params)
	if err != nil {
		return fmt.Errorf("enqueueing %d feed polls: %w", len(params), err)
	}

	// Jobs skipped as duplicates are the unique constraint doing its job, not
	// an error: it means the previous poll of that feed is still in flight.
	var inserted int
	for _, r := range results {
		if !r.UniqueSkippedAsDuplicate {
			inserted++
		}
	}

	w.log.Debug("scheduled feed polls",
		"due", len(due), "enqueued", inserted, "already_queued", len(due)-inserted)

	if len(due) == scheduleBatchSize {
		// Visible rather than silent: a persistently full batch means feeds
		// are being polled more slowly than they come due.
		w.log.Info("scheduler batch was full; remaining feeds wait for the next run",
			"batch_size", scheduleBatchSize)
	}
	return nil
}

// ScheduleFetchesArgs asks for unfetched articles to be enqueued.
type ScheduleFetchesArgs struct{}

// Kind implements river.JobArgs.
func (ScheduleFetchesArgs) Kind() string { return "schedule_fetches" }

// InsertOpts keeps overlapping scheduler runs from piling up.
func (ScheduleFetchesArgs) InsertOpts() river.InsertOpts {
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

// ScheduleFetchesWorker enqueues a fetch for every article still pending.
//
// The poller enqueues a fetch as it discovers each article, so in steady state
// this finds nothing. It exists for the two cases that matter anyway: the
// backlog of articles ingested before there was a fetcher, and
// any article whose fetch job was lost to a crash. Without it, an article that
// slipped through would sit at 'pending' forever with nothing ever looking at
// it again.
type ScheduleFetchesWorker struct {
	river.WorkerDefaults[ScheduleFetchesArgs]

	store  *store.Store
	client *river.Client[pgx.Tx]
	log    *slog.Logger
}

// Work implements river.Worker.
func (w *ScheduleFetchesWorker) Work(ctx context.Context, _ *river.Job[ScheduleFetchesArgs]) error {
	pending, err := w.store.System().PendingFetch(ctx, scheduleBatchSize)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}

	params := make([]river.InsertManyParams, 0, len(pending))
	for _, id := range pending {
		params = append(params, river.InsertManyParams{
			Args: FetchArticleArgs{ArticleID: int64(id)},
		})
	}

	results, err := w.client.InsertMany(ctx, params)
	if err != nil {
		return fmt.Errorf("enqueueing %d article fetches: %w", len(params), err)
	}

	var inserted int
	for _, r := range results {
		if !r.UniqueSkippedAsDuplicate {
			inserted++
		}
	}

	w.log.Debug("scheduled article fetches",
		"pending", len(pending), "enqueued", inserted, "already_queued", len(pending)-inserted)
	return nil
}

// ScheduleReextractionArgs asks for bodies whose rules have changed to be
// re-extracted.
type ScheduleReextractionArgs struct{}

// Kind implements river.JobArgs.
func (ScheduleReextractionArgs) Kind() string { return "schedule_reextraction" }

// InsertOpts keeps overlapping scheduler runs from piling up.
func (ScheduleReextractionArgs) InsertOpts() river.InsertOpts {
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

// ScheduleReextractionWorker enqueues extraction for bodies the current rules
// would no longer produce.
//
// The backstop to writing a rule, not the mechanism: saving a rule enqueues its
// articles immediately, and in steady state this finds nothing. It exists for the
// case that deployment makes ordinary rather than rare — the server and the worker
// are separate Deployments, so a reader can save a rule while the worker is down,
// during a rollout or a migration or after an OOM, and eagerly queued work is then
// simply never queued. Without this, that rule would appear to have been accepted
// and would never be applied to a single article.
//
// The same shape as ScheduleFetchesWorker, and for the same reason it gives.
type ScheduleReextractionWorker struct {
	river.WorkerDefaults[ScheduleReextractionArgs]

	store  *store.Store
	client *river.Client[pgx.Tx]
	log    *slog.Logger
}

// Work implements river.Worker.
func (w *ScheduleReextractionWorker) Work(ctx context.Context, _ *river.Job[ScheduleReextractionArgs]) error {
	stale, err := w.store.System().StaleBodies(ctx, scheduleBatchSize)
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}

	params := make([]river.InsertManyParams, 0, len(stale))
	for _, b := range stale {
		params = append(params, river.InsertManyParams{
			Args: ExtractArticleArgs{ArticleID: int64(b.ID), UserID: int64(b.UserID)},
		})
	}

	results, err := w.client.InsertMany(ctx, params)
	if err != nil {
		return fmt.Errorf("enqueueing %d re-extractions: %w", len(params), err)
	}

	var inserted int
	for _, r := range results {
		if !r.UniqueSkippedAsDuplicate {
			inserted++
		}
	}

	w.log.Info("scheduled re-extraction for changed rules",
		"stale", len(stale), "enqueued", inserted, "already_queued", len(stale)-inserted)

	if len(stale) == scheduleBatchSize {
		// Visible rather than silent, like the feed scheduler's full batch: a
		// persistently full one means a rule change is being worked through in
		// slices, which is expected right after an edit and worth noticing if it
		// never stops.
		w.log.Info("re-extraction batch was full; the rest wait for the next run",
			"batch_size", scheduleBatchSize)
	}
	return nil
}
