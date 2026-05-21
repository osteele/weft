package monitor

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/banner"
	"github.com/osteele/weft/internal/db"
)

func TestCheckSchemaDriftOnce_PushesBannerWhenDBIsAheadAndBinaryNotNewer(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := db.SetDBPath(dbFile)
	defer cleanup()

	database, err := db.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	// Record a migration version ahead of this binary to simulate "DB ahead".
	if _, err := database.Exec(
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (999, 1)`,
	); err != nil {
		t.Fatalf("set goose version: %v", err)
	}

	bus := banner.NewBus()
	relaunchCh := make(chan struct{}, 1)
	cfg := SchemaDriftConfig{
		Database:       database,
		Bus:            bus,
		ProcessStart:   time.Now().Add(1 * time.Hour), // force binary "older than process"
		RelaunchSignal: relaunchCh,
	}
	checkSchemaDriftOnce(cfg, cfg.ProcessStart)

	select {
	case <-relaunchCh:
		t.Fatal("relaunch should NOT be signalled when on-disk binary is not newer")
	default:
	}
	snap := bus.Snapshot()
	if len(snap) != 1 || snap[0].ID != SchemaDriftBannerID {
		t.Fatalf("expected schema-drift banner, got %+v", snap)
	}
	if snap[0].Severity != banner.SeverityCritical {
		t.Fatalf("expected critical severity, got %v", snap[0].Severity)
	}
}

func TestCheckSchemaDriftOnce_RelaunchesWhenBinaryIsNewer(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := db.SetDBPath(dbFile)
	defer cleanup()

	database, err := db.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	if _, err := database.Exec(
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (999, 1)`,
	); err != nil {
		t.Fatalf("set goose version: %v", err)
	}

	bus := banner.NewBus()
	relaunchCh := make(chan struct{}, 1)
	cfg := SchemaDriftConfig{
		Database:       database,
		Bus:            bus,
		ProcessStart:   time.Unix(0, 0), // ancient: any real binary mtime is newer
		RelaunchSignal: relaunchCh,
	}
	checkSchemaDriftOnce(cfg, cfg.ProcessStart)

	select {
	case <-relaunchCh:
	default:
		t.Fatal("relaunch should have been signalled when on-disk binary is newer")
	}
	if got := bus.Snapshot(); len(got) != 0 {
		t.Fatalf("banner should be cleared on relaunch path, got %+v", got)
	}
}

func TestCheckSchemaDriftOnce_ClearsBannerWhenSchemaInSync(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := db.SetDBPath(dbFile)
	defer cleanup()

	database, err := db.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	bus := banner.NewBus()
	bus.Push(banner.Banner{ID: SchemaDriftBannerID, Text: "stale notice"})

	cfg := SchemaDriftConfig{Database: database, Bus: bus, ProcessStart: time.Now()}
	checkSchemaDriftOnce(cfg, cfg.ProcessStart)

	if got := bus.Snapshot(); len(got) != 0 {
		t.Fatalf("expected banner cleared on healthy poll, got %+v", got)
	}
}
