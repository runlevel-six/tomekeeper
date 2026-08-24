package backup

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// dumpOrder is every table this archive backs up, in an order a restore can load
// them in: a table never appears before one it references.
//
// Written out rather than derived from the foreign keys at runtime, for two reasons.
// A topological sort is only correct while the graph is acyclic and would fail at the
// least convenient moment; and the list itself is the specification of what a backup
// contains, which is worth reading in one place.
//
// The guard that keeps it honest is knownTables: a table in the database that is not
// named here fails the backup. A migration that adds a table and forgets this list
// would otherwise produce a backup missing it — silently, and only discovered by a
// restore that needed it.
var dumpOrder = []string{
	"users",
	"categories",
	"feeds",
	"tags",
	"articles",
	"assets",
	"article_content",
	"article_state",
	"article_assets",
	"article_tags",
	"domain_rules",
	"feed_items",
	"highlights",
	"import_records",
	"password_setup_links",
}

// skipTables are the tables a backup deliberately leaves out, each for a stated
// reason.
//
// `goose_db_version` is the schema's own bookkeeping, and a restore migrates before
// it loads anything — carrying the source's rows would either duplicate what
// migration just wrote or, worse, claim a schema the restored database does not have.
//
// The `river_*` tables are the job queue. The 2026-08-19 restore drill found that a
// pg_dump carries them along, so a restored archive inherits the queue as it stood
// when the dump was taken and the worker starts chasing articles that may no longer
// exist. A queue is derivable from the archive — the schedulers re-enqueue what is
// outstanding within a minute — so rebuilding beats resurrecting.
var skipTables = map[string]string{
	"goose_db_version":   "the schema's own bookkeeping; a restore migrates first",
	"river_job":          "the job queue, which is rebuilt rather than resurrected",
	"river_leader":       "queue leadership, meaningless on another machine",
	"river_migration":    "River's own schema bookkeeping",
	"river_notification": "queue notifications, transient by nature",
	"river_queue":        "queue metadata, rebuilt on start",
}

// tableColumns lists a table's columns in ordinal order, excluding generated ones.
//
// Generated columns must be excluded from both halves of the round trip: `COPY TO`
// with an explicit column list may not name them, and `COPY FROM` may not write them.
// `article_content.tsv` and `articles.title_tsv` are both generated, so a dump that
// named every column would fail on the first table it reached.
func tableColumns(ctx context.Context, tx pgx.Tx, table string) ([]string, error) {
	rows, err := tx.Query(ctx, `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1
		  AND is_generated = 'NEVER'
		ORDER BY ordinal_position`, table)
	if err != nil {
		return nil, fmt.Errorf("listing the columns of %s: %w", table, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, fmt.Errorf("scanning a column name: %w", err)
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("table %s has no columns, which means it does not exist", table)
	}
	return cols, nil
}

// checkTablesAreKnown fails when the database holds a table this archive does not
// name, in either list.
//
// The point is the failure. A migration that adds a table is a migration that adds
// something worth backing up, and the alternative to failing here is a backup that
// looks complete and is not — the same shape as a column that is always NULL or a
// terminal state wearing a transient label, both of which this project has paid for.
func checkTablesAreKnown(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		return fmt.Errorf("listing tables: %w", err)
	}
	defer rows.Close()

	known := make(map[string]bool, len(dumpOrder))
	for _, t := range dumpOrder {
		known[t] = true
	}

	var unknown []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("scanning a table name: %w", err)
		}
		if known[name] || skipTables[name] != "" {
			continue
		}
		unknown = append(unknown, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("the database has %d table(s) this backup does not know about: %s\n"+
			"add them to dumpOrder in internal/backup/tables.go, in an order a restore can load "+
			"them in, or to skipTables with the reason they are left out",
			len(unknown), strings.Join(unknown, ", "))
	}

	// And the other direction: a table named here that no longer exists would make a
	// restore fail on a file the backup could not have written.
	for _, t := range dumpOrder {
		var exists bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM information_schema.tables
			               WHERE table_schema = 'public' AND table_name = $1)`, t).Scan(&exists); err != nil {
			return fmt.Errorf("checking that %s exists: %w", t, err)
		}
		if !exists {
			return fmt.Errorf("dumpOrder names table %q, which the database does not have; "+
				"remove it from internal/backup/tables.go", t)
		}
	}
	return nil
}
