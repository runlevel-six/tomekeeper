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
	"github.com/runlevel-six/tomekeeper/internal/render"
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

	// ImageConcurrency bounds simultaneous image transcoding, which is far more
	// expensive per call than anything else the worker does.
	ImageConcurrency int

	// RetainAfterRead mirrors the config setting. Zero means keep everything,
	// which is the default.
	RetainAfterRead time.Duration

	// Renderer drives a headless browser for the domains flagged as needing one. Nil
	// is the ordinary case and means those articles stay pending until a browser
	// exists.
	Renderer *render.Renderer

	// RenderConcurrency is how many pages may be rendered at once. Zero means one.
	RenderConcurrency int
}

// renderSlots bounds the render queue.
//
// Capped rather than trusted, because each concurrent render is a browser tab holding a
// document and its scripts — the same shape of cost as image transcoding, where the
// setting exists precisely because the obvious number is too high. Two is generous for
// an archive whose flagged domains are counted on one hand.
func renderSlots(configured int) int {
	switch {
	case configured <= 0:
		return 1
	case configured > maxRenderSlots:
		return maxRenderSlots
	default:
		return configured
	}
}

// maxRenderSlots is the ceiling on concurrent renders.
const maxRenderSlots = 4

// NewWorkerClient builds a River client that works jobs.
//
// This is the worker side. `tome serve` does not run one, and when it needs to
// enqueue work it will build an insert-only client with no Queues configured.
func NewWorkerClient(d Deps) (*river.Client[pgx.Tx], error) {
	workers := river.NewWorkers()

	river.AddWorker(workers, &PollFeedWorker{poller: d.Poller})
	river.AddWorker(workers, &FetchArticleWorker{
		store: d.Store, client: d.Client, blobs: d.Blobs, log: d.Log,
		renderer: d.Renderer,
	})

	// Registered whether or not a browser is configured, for the same reason retention's
	// worker is: an unregistered job kind is an error at run time, and a render enqueued
	// by a worker that was restarted without its browser would sit in the queue as
	// "unknown kind" rather than as work waiting for a browser. With a nil Renderer the
	// worker reports the article as pending and asks to be retried.
	river.AddWorker(workers, &RenderArticleWorker{
		store: d.Store, renderer: d.Renderer, client: d.Client, blobs: d.Blobs, log: d.Log,
	})
	river.AddWorker(workers, &ExtractArticleWorker{
		store: d.Store, blobs: d.Blobs, extractor: d.Extractor, log: d.Log,
	})
	imageSlots := d.ImageConcurrency
	if imageSlots <= 0 {
		imageSlots = 1
	}
	river.AddWorker(workers, &LocalizeAssetsWorker{
		store: d.Store, client: d.Client, blobs: d.Blobs,
		archive: archive.NewWriter(d.Blobs), log: d.Log,
		transcode: make(chan struct{}, imageSlots),
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

	reextractScheduler := &ScheduleReextractionWorker{store: d.Store, log: d.Log}
	river.AddWorker(workers, reextractScheduler)

	// Registered whether or not retention is on, so that turning the setting on
	// is a configuration change rather than a different build. With RetainAfterRead
	// at zero the worker returns immediately.
	river.AddWorker(workers, &ExpireContentWorker{
		store: d.Store, blobs: d.Blobs, retain: d.RetainAfterRead, log: d.Log,
	})

	// Registered on the same terms, and runs before expiry means anything: a
	// reader forgetting an article is what releases their claim on it.
	river.AddWorker(workers, &ForgetReadingWorker{
		store: d.Store, retain: d.RetainAfterRead, log: d.Log,
	})

	client, err := river.NewClient(riverpgxv5.New(d.Pool), &river.Config{
		Logger:  d.Log,
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: d.Concurrency},
			// Narrow, and separate from everything else: a render costs a browser tab and
			// can hang for its whole timeout, so its slots are bounded on their own rather
			// than drawn from the pool that polls feeds. One by default — see
			// TOME_RENDER_CONCURRENCY.
			RenderQueue: {MaxWorkers: renderSlots(d.RenderConcurrency)},
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
			// RunOnStart matters more here than for the others: the case this sweep
			// exists for is a rule saved while the worker was down, so the first
			// thing a worker should do on coming back is look for one.
			river.NewPeriodicJob(
				river.PeriodicInterval(ScheduleInterval),
				func() (river.JobArgs, *river.InsertOpts) { return ScheduleReextractionArgs{}, nil },
				&river.PeriodicJobOpts{RunOnStart: true},
			),
			expiryPeriodicJob(),
			forgetPeriodicJob(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("creating river client: %w", err)
	}

	feedScheduler.client = client
	fetchScheduler.client = client
	assetScheduler.client = client
	reextractScheduler.client = client
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
