package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"

	"github.com/runlevel-six/tomekeeper/internal/archive"
	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
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

// Deps is everything the worker pool needs.
type Deps struct {
	Pool      *pgxpool.Pool
	Store     *store.Store
	Poller    *feed.Poller
	Client    *httpclient.Client
	Blobs     blob.Store
	Extractor *extract.Extractor
	Log       *slog.Logger

	Concurrency int

	// RetainAfterRead mirrors the config setting. Zero means keep everything,
	// which is the default.
	RetainAfterRead time.Duration
}

// NewWorkerClient builds a River client that works jobs.
//
// This is the worker side. `tome serve` does not run one, and when it needs to
// enqueue work it will build an insert-only client with no Queues configured.
func NewWorkerClient(d Deps) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()

	river.AddWorker(workers, &PollFeedWorker{poller: d.Poller})
	river.AddWorker(workers, &FetchArticleWorker{
		store: d.Store, client: d.Client, blobs: d.Blobs, log: d.Log,
	})
	river.AddWorker(workers, &ExtractArticleWorker{
		store: d.Store, blobs: d.Blobs, extractor: d.Extractor, log: d.Log,
	})
	river.AddWorker(workers, &LocalizeAssetsWorker{
		store: d.Store, client: d.Client, blobs: d.Blobs,
		archive: archive.NewWriter(d.Blobs), log: d.Log,
	})

	// The schedulers need the client in order to enqueue, and the client needs
	// the workers in order to be constructed. River's documented way out of
	// the cycle is to register the workers first and fill in their client
	// afterwards, before the client is started.
	feedScheduler := &ScheduleFeedsWorker{store: d.Store, log: d.Log}
	river.AddWorker(workers, feedScheduler)

	fetchScheduler := &ScheduleFetchesWorker{store: d.Store, log: d.Log}
	river.AddWorker(workers, fetchScheduler)

	assetScheduler := &ScheduleAssetsWorker{store: d.Store, log: d.Log}
	river.AddWorker(workers, assetScheduler)

	// Registered whether or not retention is on, so that turning the setting on
	// is a configuration change rather than a different build. With RetainAfterRead
	// at zero the worker returns immediately.
	river.AddWorker(workers, &ExpireContentWorker{
		store: d.Store, blobs: d.Blobs, retain: d.RetainAfterRead, log: d.Log,
	})

	client, err := river.NewClient(riverpgxv5.New(d.Pool), &river.Config{
		Logger:  d.Log,
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: d.Concurrency},
		},
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(ScheduleInterval),
				func() (river.JobArgs, *river.InsertOpts) { return ScheduleFeedsArgs{}, nil },
				// Run immediately on startup so that a freshly deployed worker
				// begins polling without waiting out the first interval.
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			river.NewPeriodicJob(
				river.PeriodicInterval(ScheduleInterval),
				func() (river.JobArgs, *river.InsertOpts) { return ScheduleFetchesArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			river.NewPeriodicJob(
				river.PeriodicInterval(ScheduleInterval),
				func() (river.JobArgs, *river.InsertOpts) { return ScheduleAssetsArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			expiryPeriodicJob(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating river client: %w", err)
	}

	feedScheduler.client = client
	fetchScheduler.client = client
	assetScheduler.client = client
	return client, nil
}

// ApplyDomainRateLimits loads per-domain rate limits into the HTTP client.
//
// Called once at worker startup. A rule added later takes effect on the next
// restart, which is acceptable for something edited by hand a few times a
// year; the alternative is polling the table on every fetch.
func ApplyDomainRateLimits(ctx context.Context, s *store.Store, c *httpclient.Client, log *slog.Logger) error {
	rules, err := s.System().ListDomainRules(ctx)
	if err != nil {
		return err
	}

	var applied int
	for _, r := range rules {
		if r.RateLimitRPS > 0 {
			c.SetHostRate(r.Domain, r.RateLimitRPS)
			applied++
		}
	}
	if applied > 0 {
		log.Info("applied per-domain rate limits", "domains", applied)
	}
	return nil
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
