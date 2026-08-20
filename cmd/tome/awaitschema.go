package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/runlevel-six/tomekeeper/internal/db"
)

// awaitSchema blocks until the database has the migrations this binary needs.
//
// It exists to be an initContainer, and it exists because of a real outage on
// 2026-08-20. `tome serve` and `tome worker` both refuse to run against a schema
// older than the binary — which is correct, and which turns a mis-ordered deploy
// into a CrashLoopBackOff on the worker and a 503 readiness probe on the server.
// The Ingress then has no backend and the archive is down, for a schema change
// that takes six milliseconds whenever somebody gets around to running it.
//
// Kubernetes has no way to say "apply this Job before those Deployments" — there
// is no dependency ordering in an apply — so the ordering has to be expressed
// where it can be: at runtime, by the pod that needs it. With this in front of
// them the same mistake produces pods that sit in Init, the previous replicas
// still serving where the strategy allows it, and a log line naming the remedy.
//
// It deliberately does not migrate anything. That stays a step somebody asks for:
// a rolling restart at 3am is not when a schema change should first meet the data,
// and two replicas racing to migrate is worse than either waiting.
func awaitSchema(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("await-schema", flag.ContinueOnError)
	fs.SetOutput(stderr)
	timeout := fs.Duration("timeout", 5*time.Minute, "how long to wait before giving up")
	interval := fs.Duration("interval", 2*time.Second, "how long to wait between checks")

	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: tome await-schema [--timeout 5m] [--interval 2s]")
		fmt.Fprintln(stderr, "\nExits 0 once the database carries the migrations this build needs,")
		fmt.Fprintln(stderr, "1 if it does not within the timeout. Migrates nothing.")
		fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return exitUsage
	}
	if *interval <= 0 {
		fmt.Fprintln(stderr, "tome await-schema: --interval must be positive")
		return exitUsage
	}

	cfg, log, code := loadConfigAndLogger(stderr)
	if code != exitOK {
		return code
	}

	// The signal context comes first so that a pod being deleted while this waits
	// stops immediately rather than after the timeout. Kubernetes sends SIGTERM to
	// an initContainer like any other.
	ctx, stop := signalContext()
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()

	// A fresh connection per attempt rather than one pool held across the wait.
	// The interesting case is Postgres itself still starting, where a pool opened
	// once would be a pool that never connected — and at one attempt every two
	// seconds the cost of reconnecting is nothing.
	var lastReason string
	for attempt := 1; ; attempt++ {
		state, err := checkSchemaOnce(ctx, cfg.DatabaseURL)
		decision := evaluateSchema(state, err)

		if decision.done {
			log.Info("the database schema is current", "applied", state.Applied, "expected", state.Expected)
			fmt.Fprintf(stdout, "schema version %d is current for this build\n", state.Applied)
			return exitOK
		}

		// Logged when the reason changes rather than once per attempt: at two
		// seconds apart, a five-minute wait is 150 identical lines, and an
		// initContainer log nobody can skim is an initContainer log nobody reads.
		if decision.reason != lastReason {
			log.Info("waiting for the database schema", "reason", decision.reason, "attempt", attempt)
			lastReason = decision.reason
		}

		select {
		case <-ctx.Done():
			// The reason, not the context error: "context deadline exceeded" tells
			// an operator nothing, and this is the line they will be reading.
			log.Error("gave up waiting for the database schema",
				"reason", decision.reason, "waited", *timeout,
				"remedy", "run the migration Job: `tome migrate`")
			return exitFailure
		case <-time.After(*interval):
		}
	}
}

// checkSchemaOnce opens a connection, asks, and closes it again.
func checkSchemaOnce(ctx context.Context, databaseURL string) (db.SchemaState, error) {
	// A short budget per attempt, so a database that accepts connections and then
	// stops answering cannot consume the whole timeout in one attempt.
	attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pool, err := db.Open(attemptCtx, databaseURL)
	if err != nil {
		return db.SchemaState{}, fmt.Errorf("connecting: %w", err)
	}
	defer pool.Close()

	state, err := db.CheckSchema(attemptCtx, pool)
	if err != nil {
		return db.SchemaState{}, fmt.Errorf("reading the schema version: %w", err)
	}
	return state, nil
}

// schemaDecision is what one attempt concluded.
type schemaDecision struct {
	done   bool
	reason string // why it is not done yet, in an operator's terms
}

// evaluateSchema turns one attempt's outcome into a decision.
//
// Split out from the loop so the interesting part is testable without a database
// in a particular state — driving a real Postgres backwards through migration
// versions to exercise this would mutate schema state that every other test in
// this module shares.
//
// Every outcome except "current" is a wait rather than a failure, including the
// database being unreachable. Both things this waits for are ordinary during a
// deploy: Postgres may still be starting, and the migration Job may not have been
// scheduled yet. Distinguishing them would mean failing fast on the case that
// most often fixes itself.
func evaluateSchema(state db.SchemaState, err error) schemaDecision {
	if err != nil {
		return schemaDecision{reason: "the database is not answering yet: " + err.Error()}
	}
	if state.UpToDate() {
		return schemaDecision{done: true}
	}
	return schemaDecision{reason: fmt.Sprintf(
		"the database is at schema version %d and this build needs %d; "+
			"waiting for the migration to be applied", state.Applied, state.Expected)}
}
