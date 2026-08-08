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

func TestUpRepairsCampaignAffinityColumnAfterAppliedV24(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := Up(ctx, db); err != nil {
		t.Fatalf("initial Up: %v", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE campaigns DROP COLUMN affinity_machines`); err != nil {
		t.Fatalf("drop affinity_machines fixture: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM goose_db_version WHERE version_id > 25`); err != nil {
		t.Fatalf("rewind goose version: %v", err)
	}

	if err := Up(ctx, db); err != nil {
		t.Fatalf("repair Up: %v", err)
	}
	exists, err := columnExists(ctx, db, "campaigns", "affinity_machines")
	if err != nil {
		t.Fatalf("inspect repaired column: %v", err)
	}
	if !exists {
		t.Fatal("campaigns.affinity_machines was not restored")
	}
	if got, want := Version(ctx, db), Target(); got != want {
		t.Fatalf("Version = %d, want %d (Target)", got, want)
	}
}

func TestUpBackfillsLateTerminationIntentEndTimes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if err := Up(ctx, db); err != nil {
		t.Fatalf("initial Up: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM goose_db_version WHERE version_id > 40`); err != nil {
		t.Fatalf("rewind goose version: %v", err)
	}

	fixtures := []struct {
		id         int
		endedAt    int64
		intentJSON string
		want       int64
	}{
		{1, 500, `{"state":"succeeded","destroy_succeeded_at_unix":200}`, 200},
		{2, 150, `{"state":"succeeded","destroy_succeeded_at_unix":200}`, 150},
		{3, 500, `{"state":"destroying","destroy_succeeded_at_unix":200}`, 500},
		{4, 500, `{malformed`, 500},
		{5, 500, `{"destroy_succeeded_at_unix":200}`, 200},
	}
	for _, fixture := range fixtures {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO launches(id, status, provider, created_at, ended_at, termination_reason, termination_intent_json)
			VALUES (?, 'completed', 'vastai', 100, ?, 'completed', ?)`,
			fixture.id, fixture.endedAt, fixture.intentJSON,
		); err != nil {
			t.Fatalf("insert fixture %d: %v", fixture.id, err)
		}
	}

	if err := Up(ctx, db); err != nil {
		t.Fatalf("repair Up: %v", err)
	}
	for _, fixture := range fixtures {
		var got int64
		if err := db.QueryRowContext(ctx, `SELECT ended_at FROM launches WHERE id = ?`, fixture.id).Scan(&got); err != nil {
			t.Fatalf("query fixture %d: %v", fixture.id, err)
		}
		if got != fixture.want {
			t.Errorf("fixture %d ended_at = %d, want %d", fixture.id, got, fixture.want)
		}
	}
}
