package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/runlevel-six/tomekeeper/internal/asseturl"
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

	sessions, assetURLs, err := newCredentials(cfg, log)
	if err != nil {
		log.Error("cannot set up sessions", "error", err)
		return exitFailure
	}

	// Readiness consults the database; liveness deliberately does not. A
	// Postgres restart should take this instance out of the load balancer, not
	// get every replica killed and restarted. See docs/reference/cli.md.
	deps := server.Deps{Store: store.New(pool), Sessions: sessions, AssetURLs: assetURLs}

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
		// The same allowance the worker gets. This client tests a feed URL a reader
		// typed, which is exactly one of the paths the guard exists for: without it,
		// **Add a feed** would fetch anything this pod can reach on request.
		AllowPrivate: cfg.FetchAllowPrivate,
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

// newCredentials builds the two things derived from TOME_SESSION_KEY: the session
// store, and the signer for the image URLs that leave in a Fever response.
//
// Built together because they come from one secret and one decision about it. A
// generated key is a deliberate convenience, not an oversight: a first run should
// work with nothing but a database URL, and Tutorial 1 depends on that. The cost is
// that every restart invalidates sessions — and now also the outstanding image URLs
// in any Fever client's cache — so it says so loudly and names the setting that fixes
// it. Anything long-lived wants a configured key.
//
// Two keys, not one: the session cipher and the URL signer each derive their own from
// this secret with a different HKDF label, so neither can be used to forge the other.
func newCredentials(cfg *config.Config, log *slog.Logger) (*session.Cookie, *asseturl.Signer, error) {
	secret := []byte(cfg.SessionKey)
	if len(secret) == 0 {
		generated, err := session.GenerateKey()
		if err != nil {
			return nil, nil, err
		}
		secret = generated
		log.Warn("no session key configured, so one was generated for this process; "+
			"signing in again will be required after every restart, and archived images "+
			"already synced to a mobile client will stop loading",
			"set", config.Prefix+"SESSION_KEY")
	}

	if !cfg.CookieSecure {
		// Worth a line in the log, because it is a deliberate weakening that is
		// otherwise invisible until someone wonders why the cookie has no Secure
		// attribute.
		log.Warn("session cookies are not marked Secure, so they may travel over plain HTTP",
			"set", config.Prefix+"COOKIE_SECURE=true")
	}

	sessions, err := session.NewCookie(secret, session.DefaultTTL, cfg.CookieSecure)
	if err != nil {
		return nil, nil, err
	}

	signer, err := asseturl.NewSigner(secret, asseturl.DefaultTTL)
	if err != nil {
		return nil, nil, err
	}
	return sessions, signer, nil
}

// migrate applies the schema and seeds the single v1 user.
//
// Migrations never run automatically at server start. They run here, as
// their own command, so that a deployment can gate the rollout on them
// completing and so that two replicas starting at once cannot race.
// passwordUnchanged reports whether the configured password is already the stored
// one.
//
// Its own function so it can be tested without running a migration: the branch it
// guards is what keeps a deploy from signing the reader out, and that is not a
// property to leave unproven because the command around it is awkward to invoke.
//
// Errors are split deliberately. No such user, or no password yet, is "not
// unchanged" — there is nothing to compare and the caller should go on to write
// one. A hash that cannot be parsed is a broken row, which is also worth
// overwriting, but noisily. Anything else is a database that could not answer, and
// guessing "changed" there would rewrite the password and revoke every session on
// a transient failure.
func passwordUnchanged(ctx context.Context, system *store.SystemStore,
	username, password string, log *slog.Logger,
) (bool, error) {
	existing, err := system.Credentials(ctx, username)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	case existing.PasswordHash == "":
		return false, nil
	}

	same, err := auth.Verify(existing.PasswordHash, password)
	if err != nil {
		log.Warn("the stored password hash could not be read; replacing it", "error", err)
		return false, nil
	}
	return same, nil
}

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

	userID, name, err := system.EnsureSeedUser(ctx, cfg.Username)
	if err != nil {
		log.Error("seeding the user failed", "error", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "schema up to date; user %q is id %d\n", name, userID)

	// Said out loud, because the alternative is silence about a setting that is not
	// being obeyed. TOME_USERNAME names the account when it is created; after that the
	// reader owns their own name, and this Job used to overwrite their choice on every
	// deploy.
	if name != cfg.Username {
		fmt.Fprintf(stdout, "%sUSERNAME says %q, which is ignored: the account was renamed to %q "+
			"from Settings, and configuration does not overrule that\n",
			config.Prefix, cfg.Username, name)
	}

	// The password is set here rather than by `tome serve`, so the cleartext
	// exists only in the migration step and never in the long-running process.
	// Unset is not an error: a worker needs no login, and neither does an archive
	// nobody has signed into yet.
	if cfg.Password == "" {
		fmt.Fprintf(stdout, "no %sPASSWORD set, so no password was changed\n", config.Prefix)
		fmt.Fprintln(stdout, "the web interface cannot be signed into until one is")
		return exitOK
	}

	// An unchanged password is not a change, and this command runs on every
	// deploy.
	//
	// TOME_PASSWORD is a Secret key, so it is present every time the migration Job
	// runs. Rewriting the row unconditionally was harmless while it only replaced a
	// hash with an equivalent one and an API key with the identical one — and
	// stopped being harmless the moment setting a password also revoked sessions,
	// because it would then sign the reader out of the web interface on every
	// deployment. Verifying first is what keeps a deploy silent.
	//
	// The stored hash is what gets verified against, not a comparison of hashes:
	// argon2id salts randomly, so the same password hashes differently every time
	// and two hashes are never equal.
	//
	// **Looked up by the name the account has, not by the configured one**, and every
	// line below uses the same. Passing cfg.Username here found nothing once the reader
	// had renamed themselves: an account that cannot be found reads as "no password
	// stored", so a deploy that changed nothing decided the password had changed —
	// revoking every session and rewriting the Fever API key on every single deploy,
	// which is the exact failure the check above was added to prevent. Reported from
	// production, one release after the rename it was written to protect.
	unchanged, err := passwordUnchanged(ctx, system, name, cfg.Password, log)
	if err != nil {
		log.Error("checking the stored password failed", "error", err)
		return exitFailure
	}
	if unchanged {
		fmt.Fprintf(stdout, "the password for %q is already the configured one; nothing changed\n",
			name)
		fmt.Fprintln(stdout, "existing sessions and mobile clients keep working")
		return exitOK
	}

	hash, err := auth.Hash(cfg.Password)
	if err != nil {
		log.Error("hashing the password failed", "error", err)
		return exitFailure
	}
	// The key is md5(name:password) and the client computes it from what the reader
	// types, so it has to be derived from the name the account actually has. Derived
	// from cfg.Username it was a credential for a username that no longer existed:
	// every mobile client refused, and nothing in the archive could say why.
	if err := system.SetPassword(ctx, userID, hash, auth.FeverAPIKey(name, cfg.Password)); err != nil {
		log.Error("storing the password failed", "error", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "password set for %q\n", name)
	// Stated because both are surprising, and reached only when the password really
	// changed. The Fever key is derived from the cleartext, so it necessarily
	// changes with it; browser sessions are revoked because a password change that
	// left them signed in would not be one.
	fmt.Fprintln(stdout, "the Fever API key changed with it; mobile clients will need reconnecting")
	fmt.Fprintln(stdout, "existing browser sessions were signed out")
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
