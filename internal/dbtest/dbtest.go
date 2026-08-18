// Package dbtest provides a live PostgreSQL database for integration tests.
//
// Tests that need one call Setup, which skips when TOME_TEST_DATABASE_URL is
// unset. Skipping rather than failing is deliberate: `go test ./...` on a
// laptop with no Postgres should be useful and fast, and CI supplies the
// variable so the same tests run for real there.
//
// A skipped test is not a passing test. `task test:integration` fails loudly
// when the variable is missing, so that the coverage cannot be lost silently.
package dbtest

import (
	"context"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/runlevel-six/tomekeeper/internal/db"
	"github.com/runlevel-six/tomekeeper/internal/store"
)

// EnvVar names the connection URL for the test database.
const EnvVar = "TOME_TEST_DATABASE_URL"

// lockKey identifies the advisory lock that serializes database-backed tests.
//
// The value is arbitrary. All that matters is that every test binary in this
// module agrees on it.
const lockKey = 20260817

var (
	migrateOnce sync.Once
	migrateErr  error
)

// Setup returns a pool connected to the test database, with migrations
// applied and every table emptied.
//
// The schema is migrated once per test binary; the truncation runs per test,
// so tests are independent without paying for a migration each time.
func Setup(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv(EnvVar)
	if url == "" {
		t.Skipf("%s is not set; skipping integration test", EnvVar)
	}

	connectCtx, cancelConnect := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelConnect()

	pool, err := db.Open(connectCtx, url)
	if err != nil {
		t.Fatalf("connecting to the test database: %v", err)
	}
	t.Cleanup(pool.Close)

	// Hold the lock before migrating or truncating anything.
	lock(t, pool)

	// A fresh deadline, started *after* the wait for the lock.
	//
	// This used to share one 30-second context with the connect above, which meant
	// the clock was already running while this test queued behind another
	// package's. A test that waited its turn for longer than that then acquired the
	// lock and immediately failed the truncate with "context deadline exceeded" —
	// reported against whichever test was unlucky, in a package that had done
	// nothing wrong, and only when the suite was busy enough to queue. The lock
	// wait has its own generous budget precisely so that queueing is not an error;
	// reusing the caller's deadline for the work afterwards threw that away.
	workCtx, cancelWork := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancelWork()

	migrateOnce.Do(func() {
		migrateErr = db.Migrate(workCtx, pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	if migrateErr != nil {
		t.Fatalf("migrating the test database: %v", migrateErr)
	}

	truncate(workCtx, t, pool)
	return pool
}

// SetupWithUser is Setup plus the seeded v1 user, which almost every test
// needs because every user-scoped table references it.
func SetupWithUser(t *testing.T) (*pgxpool.Pool, *store.Store, store.UserID) {
	t.Helper()

	pool := Setup(t)
	s := store.New(pool)

	userID, err := s.System().EnsureSeedUser(t.Context(), "tome")
	if err != nil {
		t.Fatalf("seeding the test user: %v", err)
	}
	return pool, s, userID
}

// lock serializes database-backed tests across test binaries, releasing the
// lock when the test ends.
//
// `go test ./...` runs each package's test binary concurrently, and every call
// to Setup truncates the same database. Without this, one package's Setup wipes
// another package's fixtures mid-test: the jobs pipeline waits for an article
// the feed package's truncate has just deleted, and the failure surfaces as an
// unexplained timeout in whichever test lost the race, in a different package
// from the one that caused it. That is a bad afternoon to debug, and it is
// timing-dependent enough to pass locally and fail in CI.
//
// A lock rather than a database per package: the tests are short and mostly
// serial in wall-clock terms anyway, and this keeps one connection URL, one
// schema, and one migration path. Packages that do not touch the database are
// unaffected and still run in parallel.
func lock(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	// Deliberately generous, and separate from the caller's timeout: this waits
	// for other packages' tests rather than for the database, and the jobs
	// package's pipeline tests can legitimately hold it for tens of seconds.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 3*time.Minute)
	defer cancel()

	// A dedicated connection, because an advisory lock belongs to the session
	// that took it. Returning the connection to the pool mid-test would let
	// another test acquire it and unlock work it does not own.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a connection for the test lock: %v", err)
	}

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		conn.Release()
		t.Fatalf("waiting for the test lock: %v", err)
	}

	t.Cleanup(func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 30*time.Second)
		defer cancel()

		if _, err := conn.Exec(unlockCtx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
			t.Errorf("releasing the test lock: %v", err)
		}
		conn.Release()
	})
}

// truncate empties every application table.
//
// TRUNCATE with CASCADE and RESTART IDENTITY rather than DROP and re-migrate:
// it is far faster, and restarting the sequences means ids are predictable
// from one test to the next.
//
// River's own tables are deliberately left alone. They are not part of the
// application schema, and a test that cares about queue state says so.
func truncate(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	const stmt = `
		TRUNCATE TABLE
			import_records, highlights, article_tags, tags, domain_rules,
			article_assets, assets, article_state, feed_items, article_content,
			articles, feeds, users
		RESTART IDENTITY CASCADE`

	if _, err := pool.Exec(ctx, stmt); err != nil {
		t.Fatalf("truncating test tables: %v", err)
	}
}
