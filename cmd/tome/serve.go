package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/runlevel-six/tomekeeper/internal/config"
	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/logging"
	"github.com/runlevel-six/tomekeeper/internal/server"
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

	// Readiness consults the database; liveness deliberately does not. A
	// Postgres restart should take this instance out of the load balancer, not
	// get every replica killed and restarted. See docs/reference/cli.md.
	srv := server.New(cfg, log, server.Check{
		Name: "database",
		Func: func(ctx context.Context) error { return db.Ping(ctx, pool) },
	})

	if err := srv.Run(ctx); err != nil {
		log.Error("server failed", "error", err)
		return exitFailure
	}
	return exitOK
}

// migrate applies the schema and seeds the single v1 user.
//
// Migrations never run automatically at server start (§10). They run here, as
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

	userID, err := store.New(pool).System().EnsureSeedUser(ctx, cfg.Username)
	if err != nil {
		log.Error("seeding the user failed", "error", err)
		return exitFailure
	}

	fmt.Fprintf(stdout, "schema up to date; user %q is id %d\n", cfg.Username, userID)
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
