package jobs

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// InsertOpts makes the scheduler job unique, so that a slow run cannot cause
// several schedulers to overlap and enqueue the same feeds repeatedly.
func (ScheduleFeedsArgs) InsertOpts() river.InsertOpts {
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

// NewWorkerClient builds a River client that works jobs.
//
// This is the worker side. `tome serve` does not run one at M1 because it has
// nothing to enqueue yet; when it does, it will build an insert-only client
// with no Queues configured.
func NewWorkerClient(
	pool *pgxpool.Pool,
	s *store.Store,
	poller *feed.Poller,
	concurrency int,
	log *slog.Logger,
) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()

	river.AddWorker(workers, &PollFeedWorker{poller: poller})

	// The scheduler needs the client in order to enqueue, and the client needs
	// the workers in order to be constructed. River's documented way out of
	// the cycle is to register the worker first and fill in its client
	// afterwards, before the client is started.
	scheduler := &ScheduleFeedsWorker{store: s, log: log}
	river.AddWorker(workers, scheduler)

	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Logger:  log,
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: concurrency},
		},
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(ScheduleInterval),
				func() (river.JobArgs, *river.InsertOpts) { return ScheduleFeedsArgs{}, nil },
				// Run immediately on startup so that a freshly deployed worker
				// begins polling without waiting out the first interval.
				&river.PeriodicJobOpts{RunOnStart: true},
			),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating river client: %w", err)
	}

	scheduler.client = client
	return client, nil
}

// Run starts the worker client and blocks until ctx is canceled, then stops it
// gracefully.
//
// Graceful means in-flight jobs are given the chance to finish. A poll killed
// mid-write would leave the feed's validators inconsistent with what was
// actually ingested, and the next poll would then take a 304 for a feed whose
// items were never stored.
func Run(ctx context.Context, client *river.Client[pgx.Tx], log *slog.Logger) error {
	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("starting worker: %w", err)
	}
	log.Info("worker started")

	<-ctx.Done()
	log.Info("stopping worker")

	// Stop takes its own context: the one that just fired is already canceled,
	// and passing it would abandon every running job immediately.
	stopCtx := context.WithoutCancel(ctx)
	if err := client.Stop(stopCtx); err != nil {
		return fmt.Errorf("stopping worker: %w", err)
	}

	log.Info("worker stopped")
	return nil
}
