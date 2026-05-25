package db

import (
	"strings"
	"testing"
	"time"
)

// Triggers added in 00007_launch_terminal_invariants.sql:
//   * launches_terminal_auto_stamp_ended_at[_insert]
//   * launches_terminal_auto_derive_reason[_insert]
//   * launches_grace_requires_deadline_[update|insert]
//
// Tests verify the structural enforcement of the three campaign-lifecycle
// invariants (TerminalInstancesHaveEndTime, TerminalInstancesHaveReason,
// GraceRequiresDeadline) at the table layer, so a writer that forgets
// either field cannot leak the wedged state surfaced by a prod scan
// (5 missing ended_at, 49 missing termination_reason).

func TestLaunchTrigger_AutoStampEndedAt_OnTerminalTransition(t *testing.T) {
	database := setupTestDB(t)
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	// Writer forgets to stamp ended_at when transitioning to failed.
	if _, err := database.Exec(`UPDATE launches SET status = ? WHERE id = ?`, LaunchStatusFailed, id); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}

	var endedAt *int64
	if err := database.QueryRow(`SELECT ended_at FROM launches WHERE id = ?`, id).Scan(&endedAt); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if endedAt == nil || *endedAt <= 0 {
		t.Fatalf("ended_at = %v, want auto-stamped positive unix ts", endedAt)
	}
	if time.Now().Unix()-*endedAt > 5 {
		t.Fatalf("ended_at too old: %d", *endedAt)
	}
}

func TestLaunchTrigger_AutoStampEndedAt_PreservesExplicitValue(t *testing.T) {
	database := setupTestDB(t)
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	explicit := time.Now().Unix() - 3600
	if _, err := database.Exec(
		`UPDATE launches SET status = ?, ended_at = ? WHERE id = ?`,
		LaunchStatusCompleted, explicit, id,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	var endedAt int64
	if err := database.QueryRow(`SELECT ended_at FROM launches WHERE id = ?`, id).Scan(&endedAt); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if endedAt != explicit {
		t.Fatalf("ended_at = %d, want explicit %d", endedAt, explicit)
	}
}

func TestLaunchTrigger_AutoDeriveReason_OnFailed(t *testing.T) {
	database := setupTestDB(t)
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET status = ? WHERE id = ?`, LaunchStatusFailed, id); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	var reason string
	if err := database.QueryRow(`SELECT termination_reason FROM launches WHERE id = ?`, id).Scan(&reason); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if reason != "unknown" {
		t.Fatalf("termination_reason = %q, want %q (auto-derived sentinel)", reason, "unknown")
	}
}

func TestLaunchTrigger_AutoDeriveReason_OnCanceled(t *testing.T) {
	database := setupTestDB(t)
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := database.Exec(`UPDATE launches SET status = ? WHERE id = ?`, LaunchStatusCancelled, id); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	var reason string
	if err := database.QueryRow(`SELECT termination_reason FROM launches WHERE id = ?`, id).Scan(&reason); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if reason != "canceled" {
		t.Fatalf("termination_reason = %q, want %q", reason, "canceled")
	}
}

func TestLaunchTrigger_AutoDeriveReason_PreservesExplicitValue(t *testing.T) {
	database := setupTestDB(t)
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE launches SET status = ?, termination_reason = ? WHERE id = ?`,
		LaunchStatusFailed, "preempted", id,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	var reason string
	if err := database.QueryRow(`SELECT termination_reason FROM launches WHERE id = ?`, id).Scan(&reason); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if reason != "preempted" {
		t.Fatalf("termination_reason = %q, want explicit %q", reason, "preempted")
	}
}

func TestLaunchTrigger_GraceRequiresDeadline_RaisesOnMissing(t *testing.T) {
	database := setupTestDB(t)
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	_, err = database.Exec(`UPDATE launches SET status = ? WHERE id = ?`, LaunchStatusGrace, id)
	if err == nil {
		t.Fatal("expected RAISE on grace transition without grace_started_at / grace_deadline")
	}
	if !strings.Contains(err.Error(), "grace_started_at") {
		t.Fatalf("error = %v, want mention of grace_started_at", err)
	}
}

func TestLaunchTrigger_GraceRequiresDeadline_AcceptsBoth(t *testing.T) {
	database := setupTestDB(t)
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	now := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE launches SET status = ?, grace_started_at = ?, grace_deadline = ? WHERE id = ?`,
		LaunchStatusGrace, now, now+300, id,
	); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM launches WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if status != LaunchStatusGrace {
		t.Fatalf("status = %q, want grace", status)
	}
}
