package main

import (
	"io"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/runlevel-six/tomekeeper/internal/config"
	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/feed"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/jobs"
	"github.com/runlevel-six/tomekeeper/internal/store"
	"github.com/runlevel-six/tomekeeper/internal/version"
)

// worker runs the background job pool until a termination signal arrives.
//
// This is a separate process from `tome serve` because polling and, later,
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

	client, err := jobs.NewWorkerClient(pool, store.New(pool),
		newPoller(cfg, pool, log), cfg.WorkerConcurrency, log)
	if err != nil {
		log.Error("cannot start the worker", "error", err)
		return exitFailure
	}

	if err := jobs.Run(ctx, client, log); err != nil {
		log.Error("worker failed", "error", err)
		return exitFailure
	}
	return exitOK
}

// newPoller builds the feed poller from configuration. Shared by the worker
// and by anything else that needs to poll on demand.
func newPoller(cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) *feed.Poller {
	policy := feed.IntervalPolicy{
		Min:    cfg.PollMinInterval,
		Max:    cfg.PollMaxInterval,
		Growth: feed.DefaultIntervalPolicy().Growth,
	}
	client := httpclient.New(httpclient.UserAgent(version.Short(), cfg.ContactURL))

	return feed.NewPoller(store.New(pool), client, policy, cfg.FeedFailureThreshold, log)
}
