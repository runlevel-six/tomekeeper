package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

// Migrations are embedded so that the binary is self-contained. The runtime
// image is distroless — there is no shell and no filesystem to read .sql files
// from, and a migration job that depends on files shipped separately from the
// binary that expects them is a version-skew incident waiting to happen.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate brings the database up to date: first the application schema, then
// River's own job-queue tables.
//
// Ordering matters only in that both must complete before a worker starts.
// They are separate migration histories on purpose — River owns its schema and
// upgrades it on its own release schedule, and mixing the two would mean
// hand-copying River's migrations into this repository forever.
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	if err := migrateApp(ctx, pool, log); err != nil {
		return fmt.Errorf("application schema: %w", err)
	}
	if err := migrateRiver(ctx, pool, log); err != nil {
		return fmt.Errorf("river schema: %w", err)
	}
	return nil
}

func migrateApp(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("opening embedded migrations: %w", err)
	}

	// goose speaks database/sql. Closing this handle does not close the pool.
	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, sub)
	if err != nil {
		return fmt.Errorf("creating migration provider: %w", err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return err
	}

	for _, r := range results {
		log.Info("applied migration",
			"version", r.Source.Version,
			"path", r.Source.Path,
			"duration", r.Duration,
		)
	}
	if len(results) == 0 {
		log.Info("application schema already up to date")
	}
	return nil
}

func migrateRiver(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Logger: log})
	if err != nil {
		return fmt.Errorf("creating river migrator: %w", err)
	}

	res, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{})
	if err != nil {
		return err
	}

	for _, v := range res.Versions {
		log.Info("applied river migration", "version", v.Version)
	}
	if len(res.Versions) == 0 {
		log.Info("river schema already up to date")
	}
	return nil
}

// SchemaState compares the schema in the database against the one this binary
// was built with.
type SchemaState struct {
	// Applied is the highest migration version the database has.
	Applied int64

	// Expected is the highest migration version embedded in this binary.
	Expected int64
}

// UpToDate reports whether the database is new enough for this binary.
//
// Newer than expected is fine: that is a rollback in progress, and the old
// binary's queries still work against a superset schema. Older is not, because
// this binary will reference columns that do not exist.
func (s SchemaState) UpToDate() bool { return s.Applied >= s.Expected }

// CheckSchema reports whether the database has the migrations this binary needs.
//
// This exists because of a real outage. `tome serve` and `tome migrate` ship in
// one image, CI republishes that image on every green build, and a pod restart
// pulls it — so a binary can start against a schema that predates it without
// anything having gone wrong procedurally. What the reader sees is every page
// returning 500 with "column st.kept does not exist" in a log they are not
// reading.
//
// Wiring this into readiness turns that into a pod that never goes ready, an
// unchanged site still served by the old replica where possible, and a message
// naming the exact remedy. The schema is still never migrated automatically —
// that remains a deliberate step — but running without it is now loud.
func CheckSchema(ctx context.Context, pool *pgxpool.Pool) (SchemaState, error) {
	var state SchemaState

	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return state, fmt.Errorf("opening embedded migrations: %w", err)
	}

	sqlDB := stdlib.OpenDBFromPool(pool)
	defer func() { _ = sqlDB.Close() }()

	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, sub)
	if err != nil {
		return state, fmt.Errorf("creating migration provider: %w", err)
	}

	for _, src := range provider.ListSources() {
		state.Expected = max(state.Expected, src.Version)
	}

	// A database with no migration table at all reports zero rather than an
	// error: "nothing has ever been migrated" is a schema state, and it is the
	// one a first deployment is in.
	applied, err := provider.GetDBVersion(ctx)
	if err != nil {
		return state, fmt.Errorf("reading the applied schema version: %w", err)
	}
	state.Applied = applied

	return state, nil
}
