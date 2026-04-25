package orchestration

import (
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func openMigratedTestDB(t *testing.T) *sql.DB {
	t.Helper()
	tmp, err := os.CreateTemp("", "weft-orch-test-*.db")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	tmp.Close()
	cleanup := db.SetDBPath(tmp.Name())
	t.Cleanup(func() {
		cleanup()
		os.Remove(tmp.Name())
	})
	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestAutopilotRunner_AcquireReleaseHappyPath(t *testing.T) {
	database := openMigratedTestDB(t)
	r := NewAutopilotRunner(database, "test")
	if err := r.TryAcquire(); err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	state, err := db.LoadAutopilotState(database)
	if err != nil {
		t.Fatalf("LoadAutopilotState: %v", err)
	}
	if state.PassStartedAt.IsZero() {
		t.Fatal("expected PassStartedAt to be set after acquire")
	}
	if err := r.Release(time.Millisecond, "ok", nil); err != nil {
		t.Fatalf("Release: %v", err)
	}
	state, err = db.LoadAutopilotState(database)
	if err != nil {
		t.Fatalf("LoadAutopilotState after release: %v", err)
	}
	if !state.PassStartedAt.IsZero() {
		t.Error("PassStartedAt should clear on Release")
	}
	if state.LastPassSummary != "ok" {
		t.Errorf("LastPassSummary = %q, want %q", state.LastPassSummary, "ok")
	}
}

func TestAutopilotRunner_BusyOnSecondAcquire(t *testing.T) {
	database := openMigratedTestDB(t)
	r1 := NewAutopilotRunner(database, "first")
	if err := r1.TryAcquire(); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer r1.Release(0, "", nil)

	r2 := NewAutopilotRunner(database, "second")
	err := r2.TryAcquire()
	if !errors.Is(err, ErrAutopilotBusy) {
		t.Fatalf("expected ErrAutopilotBusy, got %v", err)
	}
}

func TestAutopilotRunner_PausedReturnsErr(t *testing.T) {
	database := openMigratedTestDB(t)
	if _, err := db.PauseAutopilot(database, "tester", "test"); err != nil {
		t.Fatalf("PauseAutopilot: %v", err)
	}
	r := NewAutopilotRunner(database, "test")
	err := r.TryAcquire()
	if !errors.Is(err, ErrAutopilotPaused) {
		t.Fatalf("expected ErrAutopilotPaused, got %v", err)
	}
}

func TestIsAutopilotPaused(t *testing.T) {
	database := openMigratedTestDB(t)
	if paused, err := IsAutopilotPaused(database); err != nil || paused {
		t.Fatalf("IsAutopilotPaused initial: %v %v", paused, err)
	}
	if _, err := db.PauseAutopilot(database, "", ""); err != nil {
		t.Fatalf("PauseAutopilot: %v", err)
	}
	if paused, err := IsAutopilotPaused(database); err != nil || !paused {
		t.Fatalf("IsAutopilotPaused after pause: %v %v", paused, err)
	}
}
