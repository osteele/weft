// Package migrations owns weft's database schema evolution. It uses goose
// (github.com/pressly/goose) as the migration runner.
//
// History before this package was a hand-rolled two-tier system (a monolithic
// idempotent initSchema body plus an append-only versionedMigrations list).
// That history has been squashed: the entire schema as of the squash is the
// single v1 baseline below, applied as a goose Go migration. New schema
// changes are added as ordinary goose SQL files under sql/ (NNNNN_name.sql)
// or, when they need Go logic, as additional entries in GoMigrations.
package migrations

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/pressly/goose/v3"
)

// baselineSchema is the complete schema as of the squash — every table,
// index, trigger and view. Extracted verbatim from the live database with
// `sqlite3 .schema`, then normalized so every statement is idempotent
// (CREATE ... IF NOT EXISTS). It is therefore safe to apply to the one
// pre-goose database, where every statement is a no-op.
//
//go:embed baseline_schema.sql
var baselineSchema string

// sqlMigrations holds versioned .sql migrations (v2 onward). goose scans it
// for files matching NNNNN_name.sql; non-matching files (README.md) are
// ignored.
//
//go:embed sql
var sqlMigrations embed.FS

// newProvider builds a goose provider over weft's schema: the squashed v1
// baseline (a Go migration) plus any versioned .sql migrations under sql/.
func newProvider(db *sql.DB) (*goose.Provider, error) {
	sub, err := fs.Sub(sqlMigrations, "sql")
	if err != nil {
		return nil, fmt.Errorf("migrations sub-fs: %w", err)
	}
	baseline := goose.NewGoMigration(
		1,
		&goose.GoFunc{RunDB: applyBaseline},
		&goose.GoFunc{RunDB: dropBaseline},
	)
	// Go-only at version 9: the real work is an ALTER TABLE that needs
	// a pragma_table_info pre-check to stay idempotent on the test path
	// that resets goose to 0 and re-runs every migration against an
	// already-current DB. See applyAddOnStartProbeSeenColumn.
	addProbeSeen := goose.NewGoMigration(
		9,
		&goose.GoFunc{RunDB: applyAddOnStartProbeSeenColumn},
		&goose.GoFunc{RunDB: dropAddOnStartProbeSeenColumn},
	)
	// Go-only at version 12 for the same reason as 9 — an ALTER TABLE
	// jobs ADD COLUMN that has to be conditional on pragma_table_info.
	// See applyPriceAuthorizations.
	addPriceAuthorizations := goose.NewGoMigration(
		12,
		&goose.GoFunc{RunDB: applyPriceAuthorizations},
		&goose.GoFunc{RunDB: dropPriceAuthorizations},
	)
	return goose.NewProvider(
		goose.DialectSQLite3,
		db,
		sub,
		goose.WithGoMigrations(baseline, addProbeSeen, addPriceAuthorizations),
		goose.WithDisableGlobalRegistry(true),
	)
}

// applyAddOnStartProbeSeenColumn adds launches.first_onstart_probe_seen_unix
// and its partial index. SQLite has no ALTER TABLE ... ADD COLUMN IF NOT
// EXISTS, so we ask pragma_table_info first and skip the ALTER if the column
// is already there (true for any DB that picked up the column from the
// baseline). This keeps the migration idempotent on the test path that resets
// goose to 0 and re-runs every migration against an already-current DB.
//
// See migration sql/00009_launches_onstart_probe_seen_at.sql for the
// commentary; the .sql file is intentionally a no-op so the migration index
// stays contiguous and greppable.
func applyAddOnStartProbeSeenColumn(ctx context.Context, db *sql.DB) error {
	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('launches') WHERE name = 'first_onstart_probe_seen_unix'`,
	).Scan(&exists); err != nil {
		return fmt.Errorf("inspect launches columns: %w", err)
	}
	if exists == 0 {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE launches ADD COLUMN first_onstart_probe_seen_unix INTEGER`,
		); err != nil {
			return fmt.Errorf("add first_onstart_probe_seen_unix column: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_launches_first_onstart_probe_seen
		    ON launches(first_onstart_probe_seen_unix)
		    WHERE first_onstart_probe_seen_unix IS NOT NULL`,
	); err != nil {
		return fmt.Errorf("create first_onstart_probe_seen index: %w", err)
	}
	return nil
}

func dropAddOnStartProbeSeenColumn(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx,
		`DROP INDEX IF EXISTS idx_launches_first_onstart_probe_seen`,
	); err != nil {
		return err
	}
	// SQLite ALTER TABLE DROP COLUMN exists since 3.35 but is brittle in
	// the presence of indexes and FKs. Down is best-effort; the index
	// drop above is the load-bearing part for a clean re-up.
	_, _ = db.ExecContext(ctx, `ALTER TABLE launches DROP COLUMN first_onstart_probe_seen_unix`)
	return nil
}

// applyPriceAuthorizations adds jobs.price_authorized_up_to_cents and the
// price_authorizations table + index. The ALTER TABLE needs an existence
// check because SQLite has no ADD COLUMN IF NOT EXISTS; the table and index
// already use IF NOT EXISTS so they're idempotent on their own.
//
// Two surfaces on top of these columns:
//   - jobs.price_authorized_up_to_cents — per-job override. Sticky for the
//     scoped job until cleared. Default scope for `weft job authorize-price`.
//   - price_authorizations — per (gpu_class, gpu_mem_gb) class-level sticky
//     override. Created with `weft job authorize-price --for-class`.
//     UNIQUE on the bucket so a new authorization upserts the prior one.
func applyPriceAuthorizations(ctx context.Context, db *sql.DB) error {
	var exists int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('jobs') WHERE name = 'price_authorized_up_to_cents'`,
	).Scan(&exists); err != nil {
		return fmt.Errorf("inspect jobs columns: %w", err)
	}
	if exists == 0 {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE jobs ADD COLUMN price_authorized_up_to_cents INTEGER`,
		); err != nil {
			return fmt.Errorf("add price_authorized_up_to_cents column: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS price_authorizations (
		    id           INTEGER PRIMARY KEY AUTOINCREMENT,
		    gpu_class    TEXT    NOT NULL,
		    gpu_mem_gb   INTEGER NOT NULL,
		    up_to_cents  INTEGER NOT NULL,
		    created_at   INTEGER NOT NULL,
		    created_by   TEXT,
		    note         TEXT,
		    UNIQUE(gpu_class, gpu_mem_gb)
		)`,
	); err != nil {
		return fmt.Errorf("create price_authorizations: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_price_authorizations_class
		    ON price_authorizations(gpu_class, gpu_mem_gb)`,
	); err != nil {
		return fmt.Errorf("create idx_price_authorizations_class: %w", err)
	}
	return nil
}

func dropPriceAuthorizations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_price_authorizations_class`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS price_authorizations`); err != nil {
		return err
	}
	// SQLite ALTER TABLE DROP COLUMN exists since 3.35; we do it best-effort
	// here so a re-up can re-add cleanly. Same caveats as
	// dropAddOnStartProbeSeenColumn — the table drop above is the load-
	// bearing part.
	_, _ = db.ExecContext(ctx, `ALTER TABLE jobs DROP COLUMN price_authorized_up_to_cents`)
	return nil
}

// HasPending reports whether any migration has not yet been applied to db.
func HasPending(ctx context.Context, db *sql.DB) (bool, error) {
	p, err := newProvider(db)
	if err != nil {
		return false, err
	}
	return p.HasPending(ctx)
}

// Version returns the highest migration version applied to db. It returns 0
// for a pre-goose database that has no goose version table yet.
func Version(ctx context.Context, db *sql.DB) int64 {
	p, err := newProvider(db)
	if err != nil {
		return 0
	}
	v, err := p.GetDBVersion(ctx)
	if err != nil {
		return 0
	}
	return v
}

// goMigrationVersions enumerates the versions implemented as Go migrations
// (versions whose work is not in sql/NNNNN_*.sql because they need column-
// existence checks or other logic SQL alone can't express idempotently).
// Keep in sync with the goose.WithGoMigrations call in newProvider.
var goMigrationVersions = []int64{9, 12}

// Target returns the highest migration version this binary knows about — the
// version a fully-migrated database should report. It is the v1 baseline plus
// any higher-numbered .sql migration under sql/ and any Go-migration version
// registered in goMigrationVersions.
func Target() int64 {
	target := int64(1) // the squashed baseline
	entries, err := fs.ReadDir(sqlMigrations, "sql")
	if err != nil {
		// Fall through to Go-migration scan; baseline + goMigrationVersions
		// still gives a meaningful Target even without the embed.
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(e.Name(), "_")
		if !ok {
			continue
		}
		if v, err := strconv.ParseInt(prefix, 10, 64); err == nil && v > target {
			target = v
		}
	}
	for _, v := range goMigrationVersions {
		if v > target {
			target = v
		}
	}
	return target
}

// Up applies every pending migration to db.
func Up(ctx context.Context, db *sql.DB) error {
	p, err := newProvider(db)
	if err != nil {
		return err
	}
	if _, err := p.Up(ctx); err != nil {
		return err
	}
	return nil
}

// applyBaseline creates the entire current schema. It is the v1 migration —
// the squash of the pre-goose migration history. Every statement is
// CREATE ... IF NOT EXISTS, so applying it to the existing pre-goose database
// is a no-op that simply lets goose record v1.
func applyBaseline(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, baselineSchema); err != nil {
		return fmt.Errorf("apply baseline schema: %w", err)
	}
	// Seed singleton rows the schema requires but a .schema dump does not
	// carry. INSERT OR IGNORE keeps this a no-op on the existing database.
	if _, err := db.ExecContext(ctx,
		`INSERT OR IGNORE INTO autopilot_state(id, paused) VALUES (1, 0)`,
	); err != nil {
		return fmt.Errorf("seed autopilot_state singleton: %w", err)
	}
	return nil
}

// dropBaseline is the down migration for the baseline. A squashed baseline has
// no meaningful down step; reset by deleting the database file.
func dropBaseline(ctx context.Context, db *sql.DB) error {
	return fmt.Errorf("the v1 baseline cannot be reverted; delete the database file to reset")
}
