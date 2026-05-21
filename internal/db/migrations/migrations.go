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
	return goose.NewProvider(
		goose.DialectSQLite3,
		db,
		sub,
		goose.WithGoMigrations(baseline),
		goose.WithDisableGlobalRegistry(true),
	)
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

// Target returns the highest migration version this binary knows about — the
// version a fully-migrated database should report. It is the v1 baseline plus
// any higher-numbered .sql migration under sql/.
func Target() int64 {
	target := int64(1) // the squashed baseline
	entries, err := fs.ReadDir(sqlMigrations, "sql")
	if err != nil {
		return target
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
