package main

import (
	"io"
	"log/slog"

	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/config"
	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/extract"
	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/jobs"
	"github.com/runlevel-six/tomekeeper/internal/store"
	"github.com/runlevel-six/tomekeeper/internal/version"
)

// worker runs the background job pool until a termination signal arrives.
//
// This is a separate process from `tome serve` because polling, fetching, and
// extraction are bursty and memory-hungry, and a backlog of them must not be
// able to make the reader unresponsive.
func worker(args []string, stderr io.Writer) int {
	if len(args) > 0 {
		return usageError(stderr, "worker", args[0])
	}

	cfg, log, code := loadConfigAndLogger(stderr)
	if code != exitOK {
		return code
	}
	log.Info("starting", "version", version.Short(), "config", cfg)

	ctx, stop := signalContext()
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("cannot reach the database", "error", err)
		return exitFailure
	}
	defer pool.Close()

	blobs, err := blob.NewFilesystem(cfg.BlobRoot)
	if err != nil {
		log.Error("cannot open the blob store", "error", err)
		return exitFailure
	}

	s := store.New(pool)
	client := newHTTPClient(cfg)

	// Per-domain rate limits come from domain rules, loaded once at startup.
	if err := jobs.ApplyDomainRateLimits(ctx, s, client, log); err != nil {
		log.Error("cannot load domain rules", "error", err)
		return exitFailure
	}

	riverClient, err := jobs.NewWorkerClient(jobs.Deps{
		Pool:        pool,
		Store:       s,
		Poller:      newPoller(cfg, s, client, log),
		Client:      client,
		Blobs:       blobs,
		Extractor:   extract.New(),
		Log:         log,
		Concurrency: cfg.WorkerConcurrency,
	})
	if err != nil {
		log.Error("cannot start the worker", "error", err)
		return exitFailure
	}

	if err := jobs.Run(ctx, riverClient, log); err != nil {
		log.Error("worker failed", "error", err)
		return exitFailure
	}
	return exitOK
}

// newHTTPClient builds the single outbound client from configuration.
func newHTTPClient(cfg *config.Config) *httpclient.Client {
	return httpclient.New(httpclient.Options{
		UserAgent:   httpclient.UserAgent(version.Short(), cfg.ContactURL),
		DefaultRPS:  cfg.FetchRPS,
		Concurrency: cfg.FetchConcurrency,
	})
}

// newPoller builds the feed poller from configuration.
func newPoller(cfg *config.Config, s *store.Store, client *httpclient.Client, log *slog.Logger) *feed.Poller {
	policy := feed.IntervalPolicy{
		Min:    cfg.PollMinInterval,
		Max:    cfg.PollMaxInterval,
		Growth: feed.DefaultIntervalPolicy().Growth,
	}
	return feed.NewPoller(s, client, policy, cfg.FeedFailureThreshold, log)
}
