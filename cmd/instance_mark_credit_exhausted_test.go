package cmd

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

func TestCollectMarkCreditExhaustedTargets_ExplicitIDs(t *testing.T) {
	database := db.SetupTestDB(t)
	failedID := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure, time.Now().Add(-30*time.Minute))

	targets, err := collectMarkCreditExhaustedTargets(database, []string{ids.FormatInstanceID(failedID)}, 0)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(targets) != 1 || targets[0].ID != failedID {
		t.Fatalf("targets = %+v, want [%d]", targets, failedID)
	}
}

func TestCollectMarkCreditExhaustedTargets_SinceWindow(t *testing.T) {
	database := db.SetupTestDB(t)
	recent := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure, time.Now().Add(-10*time.Minute))
	old := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure, time.Now().Add(-48*time.Hour))
	ineligible := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonDiskFull, time.Now().Add(-10*time.Minute))

	targets, err := collectMarkCreditExhaustedTargets(database, nil, 6*time.Hour)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	gotIDs := map[int64]bool{}
	for _, l := range targets {
		gotIDs[l.ID] = true
	}
	if !gotIDs[recent] {
		t.Errorf("recent eligible launch %d missing", recent)
	}
	if gotIDs[old] {
		t.Errorf("old launch %d outside window should be filtered out", old)
	}
	if gotIDs[ineligible] {
		t.Errorf("ineligible (disk_full) launch %d should be filtered out", ineligible)
	}
}

func TestCollectMarkCreditExhaustedTargets_UnionAndDedup(t *testing.T) {
	database := db.SetupTestDB(t)
	inWindow := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure, time.Now().Add(-30*time.Minute))
	explicit := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonProviderFailure, time.Now().Add(-72*time.Hour))

	// Specifying inWindow as both explicit and via --since should dedup.
	targets, err := collectMarkCreditExhaustedTargets(database,
		[]string{ids.FormatInstanceID(inWindow), ids.FormatInstanceID(explicit)},
		1*time.Hour)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2 (union with dedup)", len(targets))
	}
	gotIDs := map[int64]bool{}
	for _, l := range targets {
		gotIDs[l.ID] = true
	}
	if !gotIDs[inWindow] || !gotIDs[explicit] {
		t.Fatalf("missing IDs: got %v, want both %d and %d", gotIDs, inWindow, explicit)
	}
}

func TestCollectMarkCreditExhaustedTargets_RefusesNonTerminalExplicit(t *testing.T) {
	database := db.SetupTestDB(t)
	runningID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	_, err = collectMarkCreditExhaustedTargets(database, []string{ids.FormatInstanceID(runningID)}, 0)
	if err == nil {
		t.Fatalf("expected error for running launch, got nil")
	}
	if !strings.Contains(err.Error(), "failed/canceled") {
		t.Fatalf("error = %v, want message about failed/canceled", err)
	}
}

func TestCollectMarkCreditExhaustedTargets_RefusesIneligibleExplicit(t *testing.T) {
	database := db.SetupTestDB(t)
	diskFullID := makeReclassifyFixture(t, database, db.LaunchStatusFailed, db.TerminationReasonDiskFull, time.Now().Add(-30*time.Minute))

	_, err := collectMarkCreditExhaustedTargets(database, []string{ids.FormatInstanceID(diskFullID)}, 0)
	if err == nil {
		t.Fatalf("expected error for disk_full launch, got nil")
	}
	if !strings.Contains(err.Error(), "not eligible") {
		t.Fatalf("error = %v, want message about ineligibility", err)
	}
}

func TestCollectMarkCreditExhaustedTargets_NoMatches(t *testing.T) {
	database := db.SetupTestDB(t)
	// No fixtures created.
	targets, err := collectMarkCreditExhaustedTargets(database, nil, 1*time.Hour)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets = %d, want 0", len(targets))
	}
}

func makeReclassifyFixture(t *testing.T, database *sql.DB, status, reason string, ended time.Time) int64 {
	t.Helper()
	id, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusPlanned,
		Provider: "vastai",
		GPUSpec:  "RTX_3090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE launches SET status = ?, ended_at = ?, termination_reason = ? WHERE id = ?`,
		status, ended.Unix(), reason, id,
	); err != nil {
		t.Fatalf("update fixture: %v", err)
	}
	return id
}
