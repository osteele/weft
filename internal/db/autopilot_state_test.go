package db

import (
	"errors"
	"testing"
	"time"
)

func TestAutopilotState_LoadInitializesSingleton(t *testing.T) {
	database := setupTestDB(t)

	state, err := LoadAutopilotState(database)
	if err != nil {
		t.Fatalf("LoadAutopilotState: %v", err)
	}
	if state == nil {
		t.Fatal("LoadAutopilotState returned nil")
	}
	if state.Paused {
		t.Error("fresh state should not be paused")
	}
	if state.ActiveRunnerPID != 0 {
		t.Errorf("ActiveRunnerPID = %d, want 0", state.ActiveRunnerPID)
	}
	if !state.PassStartedAt.IsZero() {
		t.Error("PassStartedAt should be zero on fresh row")
	}
}

func TestAutopilotState_PauseAndResume(t *testing.T) {
	database := setupTestDB(t)

	state, err := PauseAutopilot(database, "alice", "manual launch")
	if err != nil {
		t.Fatalf("PauseAutopilot: %v", err)
	}
	if !state.Paused {
		t.Fatal("expected Paused=true after PauseAutopilot")
	}
	if state.PausedBy != "alice" {
		t.Errorf("PausedBy = %q, want %q", state.PausedBy, "alice")
	}
	if state.PausedReason != "manual launch" {
		t.Errorf("PausedReason = %q, want %q", state.PausedReason, "manual launch")
	}
	if state.PausedAt.IsZero() {
		t.Error("PausedAt should be set")
	}

	state, err = ResumeAutopilot(database)
	if err != nil {
		t.Fatalf("ResumeAutopilot: %v", err)
	}
	if state.Paused {
		t.Fatal("expected Paused=false after ResumeAutopilot")
	}
}

func TestAutopilotState_TryClaimContention(t *testing.T) {
	database := setupTestDB(t)

	claimed, paused, _, err := TryClaimAutopilotPass(database, 100, "list-tui", "host-a", 30*time.Second)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if !claimed {
		t.Fatal("first claim should succeed")
	}
	if paused {
		t.Fatal("first claim returned paused=true unexpectedly")
	}

	claimed, paused, existing, err := TryClaimAutopilotPass(database, 200, "watch-tui", "host-a", 30*time.Second)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if claimed {
		t.Fatal("second claim should fail while first holds")
	}
	if paused {
		t.Fatal("second claim returned paused=true unexpectedly")
	}
	if existing == nil {
		t.Fatal("expected existing state on contention")
	}
	if existing.ActiveRunnerPID != 100 {
		t.Errorf("existing.ActiveRunnerPID = %d, want 100", existing.ActiveRunnerPID)
	}
}

func TestAutopilotState_TryClaimRespectsPause(t *testing.T) {
	database := setupTestDB(t)

	if _, err := PauseAutopilot(database, "alice", ""); err != nil {
		t.Fatalf("PauseAutopilot: %v", err)
	}
	claimed, paused, state, err := TryClaimAutopilotPass(database, 100, "list-tui", "host-a", 30*time.Second)
	if err != nil {
		t.Fatalf("TryClaim while paused: %v", err)
	}
	if claimed {
		t.Fatal("claim should fail while paused")
	}
	if !paused {
		t.Fatal("claim should report paused=true")
	}
	if state == nil || !state.Paused {
		t.Fatal("expected returned state to reflect Paused=true")
	}
}

func TestAutopilotState_StaleClaimReclaimable(t *testing.T) {
	database := setupTestDB(t)

	// Take a claim.
	claimed, _, _, err := TryClaimAutopilotPass(database, 100, "list-tui", "host-a", 30*time.Second)
	if err != nil || !claimed {
		t.Fatalf("seed claim: %v / %v", err, claimed)
	}
	// Manually age the heartbeat past the threshold.
	staleTS := time.Now().Add(-2 * time.Minute).Unix()
	if _, err := database.Exec(`UPDATE autopilot_state SET pass_started_at=?, last_heartbeat=? WHERE id=1`, staleTS, staleTS); err != nil {
		t.Fatalf("age heartbeat: %v", err)
	}

	claimed, _, _, err = TryClaimAutopilotPass(database, 200, "watch-tui", "host-b", 30*time.Second)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if !claimed {
		t.Fatal("expected reclaim after stale heartbeat")
	}
	state, err := LoadAutopilotState(database)
	if err != nil {
		t.Fatalf("LoadAutopilotState: %v", err)
	}
	if state.ActiveRunnerPID != 200 {
		t.Errorf("ActiveRunnerPID = %d, want 200", state.ActiveRunnerPID)
	}
}

func TestAutopilotState_HeartbeatRequiresOwnership(t *testing.T) {
	database := setupTestDB(t)

	if _, _, _, err := TryClaimAutopilotPass(database, 100, "list-tui", "host-a", 30*time.Second); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := HeartbeatAutopilotPass(database, 100); err != nil {
		t.Fatalf("heartbeat owner: %v", err)
	}
	err := HeartbeatAutopilotPass(database, 999)
	if !errors.Is(err, ErrAutopilotPassLost) {
		t.Fatalf("expected ErrAutopilotPassLost for non-owner heartbeat, got %v", err)
	}
}

func TestAutopilotState_ReleaseRecordsSummary(t *testing.T) {
	database := setupTestDB(t)

	if _, _, _, err := TryClaimAutopilotPass(database, 100, "list-tui", "host-a", 30*time.Second); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := ReleaseAutopilotPass(database, 100, 1500*time.Millisecond, "placed=2 launched=1", nil); err != nil {
		t.Fatalf("release: %v", err)
	}
	state, err := LoadAutopilotState(database)
	if err != nil {
		t.Fatalf("LoadAutopilotState: %v", err)
	}
	if state.ActiveRunnerPID != 0 {
		t.Error("ActiveRunnerPID should be cleared after release")
	}
	if !state.PassStartedAt.IsZero() {
		t.Error("PassStartedAt should be cleared after release")
	}
	if state.LastPassDurationMS != 1500 {
		t.Errorf("LastPassDurationMS = %d, want 1500", state.LastPassDurationMS)
	}
	if state.LastPassSummary != "placed=2 launched=1" {
		t.Errorf("LastPassSummary = %q", state.LastPassSummary)
	}
	if state.LastPassFinishedAt.IsZero() {
		t.Error("LastPassFinishedAt should be set after release")
	}
}

func TestAutopilotState_IsActiveAndIsStale(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	stale := 30 * time.Second

	cases := []struct {
		name    string
		started time.Time
		hb      time.Time
		active  bool
		isStale bool
	}{
		{"empty", time.Time{}, time.Time{}, false, false},
		{"fresh", now.Add(-5 * time.Second), now.Add(-2 * time.Second), true, false},
		{"stale heartbeat", now.Add(-90 * time.Second), now.Add(-60 * time.Second), false, true},
		{"started no heartbeat fresh", now.Add(-5 * time.Second), time.Time{}, false, false},
		{"started no heartbeat stale", now.Add(-90 * time.Second), time.Time{}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &AutopilotState{PassStartedAt: tc.started, LastHeartbeat: tc.hb}
			if got := s.IsActive(now, stale); got != tc.active {
				t.Errorf("IsActive = %v, want %v", got, tc.active)
			}
			if got := s.IsStale(now, stale); got != tc.isStale {
				t.Errorf("IsStale = %v, want %v", got, tc.isStale)
			}
		})
	}
}
