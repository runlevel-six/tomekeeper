package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/runlevel-six/tomekeeper/internal/auth"
	"github.com/runlevel-six/tomekeeper/internal/blob"
	"github.com/runlevel-six/tomekeeper/internal/config"
	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/httpclient"
	"github.com/runlevel-six/tomekeeper/internal/logging"
	"github.com/runlevel-six/tomekeeper/internal/metrics"
	"github.com/runlevel-six/tomekeeper/internal/server"
	"github.com/runlevel-six/tomekeeper/internal/session"
	"github.com/runlevel-six/tomekeeper/internal/store"
	"github.com/runlevel-six/tomekeeper/internal/version"
)

// serve loads configuration, connects to the database, and runs the HTTP
// server until a termination signal arrives.
func serve(args []string, stderr io.Writer) int {
	if len(args) > 0 {
		return usageError(stderr, "serve", args[0])
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

	sessions, err := newSessionStore(cfg, log)
	if err != nil {
		log.Error("cannot set up sessions", "error", err)
		return exitFailure
	}

	// Readiness consults the database; liveness deliberately does not. A
	// Postgres restart should take this instance out of the load balancer, not
	// get every replica killed and restarted. See docs/reference/cli.md.
	deps := server.Deps{Store: store.New(pool), Sessions: sessions}

	// Insert-only, and the distinction is the whole point: the web interface queues
	// re-extraction when somebody presses reprocess on a domain rule, and processes
	// none of it. `tome reextract` has exactly this role. A failure to open the queue
	// costs that one control rather than the service, because a reader whose archive
	// is fine should not lose the interface over a button they may never press.
	if jobClient, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: log}); err != nil {
		log.Warn("the job queue is unavailable, so re-extraction cannot be queued from the interface",
			"error", err)
	} else {
		deps.Jobs = jobClient
	}

	// The one outbound request the reader-facing process makes: testing a feed URL
	// somebody is about to subscribe to. Its own client with a small concurrency
	// rather than the worker's, because this is not a fetcher and should not be able
	// to become one by configuration — polling, article fetches and images all stay
	// with the worker.
	deps.Fetch = httpclient.New(httpclient.Options{
		UserAgent:   httpclient.UserAgent(version.Short(), cfg.ContactURL),
		DefaultRPS:  cfg.FetchRPS,
		Concurrency: 2,
	})

	// A blob root that cannot be opened costs the reader images, not the whole
	// interface: the pages still work and the log says why, which beats refusing
	// to start over a directory the worker may create on its next run.
	//
	// Assigned only on success, deliberately. A failed constructor returns a typed
	// nil pointer, and putting that in an interface field yields an interface that
	// is not nil while holding nothing — so the handler's nil check would pass and
	// the first request would panic instead of returning a 404.
	if blobs, err := blob.NewFilesystem(cfg.BlobRoot); err != nil {
		log.Warn("the archive directory is unavailable, so stored images will not load",
			"blob_root", cfg.BlobRoot, "error", err)
	} else {
		deps.Blobs = blobs
	}

	srv := server.New(cfg, log, deps,
		server.Check{
			Name: "database",
			Func: func(ctx context.Context) error { return db.Ping(ctx, pool) },
		},
		// Readiness, not startup. Refusing to boot would mean a crash loop with
		// the reason buried in a restarting container's logs; failing readiness
		// keeps the process up and answering, with the remedy readable at
		// /readyz and in one warning per probe.
		server.Check{
			Name: "schema",
			Func: func(ctx context.Context) error {
				state, err := db.CheckSchema(ctx, pool)
				if err != nil {
					return err
				}
				if !state.UpToDate() {
					return fmt.Errorf(
						"the database is at schema version %d but this build needs %d; "+
							"run `tome migrate` (the migration Job, on Kubernetes) before serving",
						state.Applied, state.Expected)
				}
				return nil
			},
		},
	)

	// Said once at startup as well, because a reader reporting "every page is an
	// error" should be answerable from the log without anyone thinking to curl a
	// readiness endpoint.
	if state, err := db.CheckSchema(ctx, pool); err != nil {
		log.Warn("could not determine the schema version", "error", err)
	} else if !state.UpToDate() {
		log.Error("the database schema is older than this build; pages that use new columns will fail",
			"applied", state.Applied, "expected", state.Expected,
			"remedy", "run `tome migrate`")
	}

	// Metrics run beside the server rather than inside it, on their own port. An
	// archive that cannot publish metrics is still an archive, so a failure here
	// is logged and the reader is served anyway.
	stopMetrics := startMetrics(ctx, cfg, pool, log)
	defer stopMetrics()

	if err := srv.Run(ctx); err != nil {
		log.Error("server failed", "error", err)
		return exitFailure
	}
	return exitOK
}

// startMetrics runs the Prometheus listener in the background and returns a
// function that waits for it to stop.
func startMetrics(ctx context.Context, cfg *config.Config, pool *pgxpool.Pool, log *slog.Logger) func() {
	if cfg.MetricsAddr == "" {
		log.Info("metrics are disabled", "set", config.Prefix+"METRICS_ADDR")
		return func() {}
	}

	reg := metrics.New(pool, log)
	done := make(chan struct{})

	go func() {
		defer close(done)
		if err := reg.Serve(ctx, cfg.MetricsAddr, log); err != nil {
			log.Error("the metrics listener stopped", "error", err)
		}
	}()

	return func() { <-done }
}

// newSessionStore builds the session store, generating a key if none is set.
//
// A generated key is a deliberate convenience, not an oversight: a first run
// should work with nothing but a database URL, and Tutorial 1 depends on that. The
// cost is that every restart invalidates sessions, so it says so loudly and names
// the setting that fixes it. Anything long-lived wants a configured key.
func newSessionStore(cfg *config.Config, log *slog.Logger) (*session.Cookie, error) {
	secret := []byte(cfg.SessionKey)
	if len(secret) == 0 {
		generated, err := session.GenerateKey()
		if err != nil {
			return nil, err
		}
		secret = generated
		log.Warn("no session key configured, so one was generated for this process; "+
			"signing in again will be required after every restart",
			"set", config.Prefix+"SESSION_KEY")
	}

	if !cfg.CookieSecure {
		// Worth a line in the log, because it is a deliberate weakening that is
		// otherwise invisible until someone wonders why the cookie has no Secure
		// attribute.
		log.Warn("session cookies are not marked Secure, so they may travel over plain HTTP",
			"set", config.Prefix+"COOKIE_SECURE=true")
	}

	return session.NewCookie(secret, session.DefaultTTL, cfg.CookieSecure)
}

// migrate applies the schema and seeds the single v1 user.
//
// Migrations never run automatically at server start. They run here, as
// their own command, so that a deployment can gate the rollout on them
// completing and so that two replicas starting at once cannot race.
func migrate(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		return usageError(stderr, "migrate", args[0])
	}

	cfg, log, code := loadConfigAndLogger(stderr)
	if code != exitOK {
		return code
	}

	ctx, stop := signalContext()
	defer stop()

	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Error("cannot reach the database", "error", err)
		return exitFailure
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, log); err != nil {
		log.Error("migration failed", "error", err)
		return exitFailure
	}

	system := store.New(pool).System()

	userID, err := system.EnsureSeedUser(ctx, cfg.Username)
	if err != nil {
		log.Error("seeding the user failed", "error", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "schema up to date; user %q is id %d\n", cfg.Username, userID)

	// The password is set here rather than by `tome serve`, so the cleartext
	// exists only in the migration step and never in the long-running process.
	// Unset is not an error: a worker needs no login, and neither does an archive
	// nobody has signed into yet.
	if cfg.Password == "" {
		fmt.Fprintf(stdout, "no %sPASSWORD set, so no password was changed\n", config.Prefix)
		fmt.Fprintln(stdout, "the web interface cannot be signed into until one is")
		return exitOK
	}

	hash, err := auth.Hash(cfg.Password)
	if err != nil {
		log.Error("hashing the password failed", "error", err)
		return exitFailure
	}
	if err := system.SetPassword(ctx, userID, hash, auth.FeverAPIKey(cfg.Username, cfg.Password)); err != nil {
		log.Error("storing the password failed", "error", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "password set for %q\n", cfg.Username)
	// Stated every time, because it is surprising and because the Fever API design makes it
	// unavoidable: the Fever credential is derived from the cleartext, so it
	// necessarily changes with it.
	fmt.Fprintln(stdout, "the Fever API key changed with it; mobile clients will need reconnecting")
	return exitOK
}

// loadConfigAndLogger is the startup preamble every subcommand shares.
func loadConfigAndLogger(stderr io.Writer) (*config.Config, *slog.Logger, int) {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		// Plain text, not the structured logger: the logger's own settings are
		// among the things that may have just failed to validate, and a human
		// is reading this in a terminal or a crash-loop log.
		fmt.Fprintf(stderr, "tome: %v\n\n", err)
		fmt.Fprintln(stderr, "See docs/reference/configuration.md for every setting.")
		return nil, nil, exitUsage
	}
	return cfg, logging.New(stderr, cfg.LogFormat, cfg.LogLevel), exitOK
}

func usageError(stderr io.Writer, command, arg string) int {
	fmt.Fprintf(stderr, "tome %s: unexpected argument %q\n", command, arg)
	fmt.Fprintf(stderr, "tome %s takes no flags; it is configured entirely by %s* environment variables.\n",
		command, config.Prefix)
	fmt.Fprintln(stderr, "See docs/reference/configuration.md.")
	return exitUsage
}
