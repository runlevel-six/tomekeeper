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

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatalf("connecting to the test database: %v", err)
	}
	t.Cleanup(pool.Close)

	migrateOnce.Do(func() {
		migrateErr = db.Migrate(ctx, pool, slog.New(slog.NewTextHandler(io.Discard, nil)))
	})
	if migrateErr != nil {
		t.Fatalf("migrating the test database: %v", migrateErr)
	}

	truncate(ctx, t, pool)
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
