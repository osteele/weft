package monitor

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/banner"
	"github.com/osteele/weft/internal/db"
)

func TestWatchAutopilotPaused_PushesAndClears(t *testing.T) {
	dbFile := filepath.Join(t.TempDir(), "jobs.db")
	cleanup := db.SetDBPath(dbFile)
	defer cleanup()

	database, err := db.Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	if _, err := db.PauseAutopilot(database, "tester", "manual launch"); err != nil {
		t.Fatalf("PauseAutopilot: %v", err)
	}

	bus := banner.NewBus()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go WatchAutopilotPaused(ctx, AutopilotPausedConfig{
		Database: database,
		Bus:      bus,
		Interval: 10 * time.Millisecond,
	})

	if !waitForBanner(bus, AutopilotPausedBannerID, 1*time.Second) {
		t.Fatal("banner not pushed within timeout")
	}
	snap := bus.Snapshot()
	if !strings.Contains(snap[0].Text, "manual launch") {
		t.Fatalf("banner text missing reason: %q", snap[0].Text)
	}

	if _, err := db.ResumeAutopilot(database); err != nil {
		t.Fatalf("ResumeAutopilot: %v", err)
	}
	if !waitForCleared(bus, AutopilotPausedBannerID, 1*time.Second) {
		t.Fatal("banner not cleared after resume within timeout")
	}
}

func waitForBanner(bus *banner.Bus, id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, b := range bus.Snapshot() {
			if b.ID == id {
				return true
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func waitForCleared(bus *banner.Bus, id string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		found := false
		for _, b := range bus.Snapshot() {
			if b.ID == id {
				found = true
				break
			}
		}
		if !found {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
