package migrations

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestUpAppliesBaseline(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	pending, err := HasPending(ctx, db)
	if err != nil {
		t.Fatalf("HasPending: %v", err)
	}
	if !pending {
		t.Fatal("fresh DB should have pending migrations")
	}

	if err := Up(ctx, db); err != nil {
		t.Fatalf("Up: %v", err)
	}

	pending, err = HasPending(ctx, db)
	if err != nil {
		t.Fatalf("HasPending after Up: %v", err)
	}
	if pending {
		t.Fatal("no migrations should be pending after Up")
	}
	if got, want := Version(ctx, db), Target(); got != want {
		t.Fatalf("Version = %d, want %d (Target)", got, want)
	}

	// The baseline created the schema.
	for _, table := range []string{"jobs", "launches", "job_attempts", "execution_targets"} {
		var n int
		if err := db.QueryRow(
			`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table,
		).Scan(&n); err != nil {
			t.Fatalf("query %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s not created by baseline", table)
		}
	}

	// Re-running Up is a no-op.
	if err := Up(ctx, db); err != nil {
		t.Fatalf("second Up: %v", err)
	}
}

// TestUpIsIdempotentOnPopulatedSchema models the one existing pre-goose
// database: every table already exists, and applying the baseline must be a
// harmless no-op that simply records v1.
func TestUpIsIdempotentOnPopulatedSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Pre-create the schema directly, as if this were the pre-goose DB.
	if _, err := db.ExecContext(ctx, baselineSchema); err != nil {
		t.Fatalf("pre-create schema: %v", err)
	}

	if err := Up(ctx, db); err != nil {
		t.Fatalf("Up on populated schema: %v", err)
	}
	if got, want := Version(ctx, db), Target(); got != want {
		t.Fatalf("Version = %d, want %d (Target)", got, want)
	}
}
