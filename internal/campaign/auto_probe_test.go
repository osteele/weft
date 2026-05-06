package campaign

import (
	"fmt"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// TestMaybeAutoResumeBreaker_NoOpWhenNoProbe: returns nil when no probe
// has been launched.
func TestMaybeAutoResumeBreaker_NoOpWhenNoProbe(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := MaybeAutoResumeBreaker(database); err != nil {
		t.Fatalf("MaybeAutoResumeBreaker: %v", err)
	}
}

// TestMaybeAutoResumeBreaker_ResumesAfterProbeCompletes: a tripped
// breaker is auto-reset once the most recent probe launch shows
// status='completed'.
func TestMaybeAutoResumeBreaker_ResumesAfterProbeCompletes(t *testing.T) {
	database := db.SetupTestDB(t)

	// Trip the breaker globally.
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchRunawayTripped,
		Detail:    "project=<all>; test trip",
	}); err != nil {
		t.Fatalf("trip event: %v", err)
	}

	// Insert a completed launch and a probe-launched event referencing it.
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusCompleted,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchAutoProbeLaunched,
		LaunchID:  launchID,
		Detail:    fmt.Sprintf("launch_id=%d", launchID),
	}); err != nil {
		t.Fatalf("probe-launched event: %v", err)
	}

	if err := MaybeAutoResumeBreaker(database); err != nil {
		t.Fatalf("MaybeAutoResumeBreaker: %v", err)
	}

	// Verify a resumed event was inserted (we count rows because the
	// trip and the auto-resume can land in the same second).
	var resumes int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM lifecycle_events WHERE event_kind = ?`,
		db.EventRelaunchRunawayResumed,
	).Scan(&resumes); err != nil {
		t.Fatalf("count resume events: %v", err)
	}
	if resumes != 1 {
		t.Errorf("expected 1 runaway_resumed event, got %d", resumes)
	}
	var auto int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM lifecycle_events WHERE event_kind = ?`,
		db.EventRelaunchAutoProbeResumed,
	).Scan(&auto); err != nil {
		t.Fatalf("count auto-probe-resumed events: %v", err)
	}
	if auto != 1 {
		t.Errorf("expected 1 auto_probe_resumed audit event, got %d", auto)
	}
}

// A completed job_attempt under a failed launch should still resume the
// breaker — agent silence during finalization can fail the launch after
// a job exited 0.
func TestMaybeAutoResumeBreaker_ResumesWhenProbeJobCompleted(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchRunawayTripped,
		Detail:    "project=<all>; test trip",
	}); err != nil {
		t.Fatalf("trip event: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", "/tmp/x", "echo ok", "probe job")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO job_attempts (job_id, attempt_number, status, launch_id, queued_at, start_time, end_time)
		 VALUES (?, 1, 'completed', ?, strftime('%s','now')-30, strftime('%s','now')-25, strftime('%s','now')-5)`,
		jobID, launchID,
	); err != nil {
		t.Fatalf("insert attempt: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchAutoProbeLaunched,
		LaunchID:  launchID,
		Detail:    fmt.Sprintf("launch_id=%d", launchID),
	}); err != nil {
		t.Fatalf("probe-launched event: %v", err)
	}
	if err := MaybeAutoResumeBreaker(database); err != nil {
		t.Fatalf("MaybeAutoResumeBreaker: %v", err)
	}
	var resumes int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM lifecycle_events WHERE event_kind = ?`,
		db.EventRelaunchRunawayResumed,
	).Scan(&resumes); err != nil {
		t.Fatalf("count resume events: %v", err)
	}
	if resumes != 1 {
		t.Fatalf("expected 1 runaway_resumed event after probe-job completion (launch was failed), got %d", resumes)
	}
}

func TestProvidersFromTripDetail(t *testing.T) {
	cases := []struct {
		name   string
		detail string
		want   map[string]int
	}{
		{"empty", "no providers here", map[string]int{}},
		{"single", "...; providers=vastai:7", map[string]int{"vastai": 7}},
		{"multi", "...; providers=runpod:0,vastai:5", map[string]int{"runpod": 0, "vastai": 5}},
		{"trailing-semi", "x; providers=vastai:3; window=24h", map[string]int{"vastai": 3}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := providersFromTripDetail(tc.detail)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got[%q] = %d, want %d", k, got[k], v)
				}
			}
		})
	}
}

// TestMaybeAutoResumeBreaker_NoOpWhileProbeRunning: probe in flight
// (running, not completed) leaves the breaker tripped.
func TestMaybeAutoResumeBreaker_NoOpWhileProbeRunning(t *testing.T) {
	database := db.SetupTestDB(t)

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchRunawayTripped,
		Detail:    "project=<all>; test trip",
	}); err != nil {
		t.Fatalf("trip event: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchAutoProbeLaunched,
		LaunchID:  launchID,
		Detail:    fmt.Sprintf("launch_id=%d", launchID),
	}); err != nil {
		t.Fatalf("probe-launched event: %v", err)
	}

	if err := MaybeAutoResumeBreaker(database); err != nil {
		t.Fatalf("MaybeAutoResumeBreaker: %v", err)
	}

	resumedAt := latestRunawayEventAt(database, db.EventRelaunchRunawayResumed, 0, "")
	if resumedAt != 0 {
		t.Errorf("expected no resume while probe is running, got resumedAt=%d", resumedAt)
	}
}

// TestMaybeAutoResumeBreaker_NoOpWhenAlreadyResumed: if the breaker was
// already manually reset, don't re-emit a resumed event for an old
// completed probe.
func TestMaybeAutoResumeBreaker_NoOpWhenAlreadyResumed(t *testing.T) {
	database := db.SetupTestDB(t)

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchRunawayTripped,
		Detail:    "project=<all>; test trip",
	}); err != nil {
		t.Fatalf("trip event: %v", err)
	}
	if err := ResetGlobalRunawayBreaker(database, "test"); err != nil {
		t.Fatalf("ResetGlobalRunawayBreaker: %v", err)
	}
	beforeResumed := latestRunawayEventAt(database, db.EventRelaunchRunawayResumed, 0, "")
	launchID, _ := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusCompleted, Provider: "vastai",
	})
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchAutoProbeLaunched,
		LaunchID:  launchID,
		Detail:    fmt.Sprintf("launch_id=%d", launchID),
	}); err != nil {
		t.Fatalf("probe-launched event: %v", err)
	}
	if err := MaybeAutoResumeBreaker(database); err != nil {
		t.Fatalf("MaybeAutoResumeBreaker: %v", err)
	}
	afterResumed := latestRunawayEventAt(database, db.EventRelaunchRunawayResumed, 0, "")
	if afterResumed != beforeResumed {
		t.Errorf("expected no new resume event when breaker not currently tripped; before=%d after=%d", beforeResumed, afterResumed)
	}
}

// TestMaybeLaunchAutoProbe_RespectsInterval: the function does not
// launch a probe if the last probe was within AutoProbeInterval. This
// is the rate limit that bounds probe spend during sustained outages.
func TestMaybeLaunchAutoProbe_RespectsInterval(t *testing.T) {
	database := db.SetupTestDB(t)

	// Trip the breaker.
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchRunawayTripped,
		Detail:    "project=<all>; test trip",
	}); err != nil {
		t.Fatalf("trip event: %v", err)
	}
	// Recent probe.
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchAutoProbeLaunched,
		LaunchID:  1,
		Detail:    "launch_id=1",
	}); err != nil {
		t.Fatalf("probe event: %v", err)
	}

	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:           true,
			Window:            24 * time.Hour,
			AutoProbeInterval: 1 * time.Hour,
		},
	}
	probeID, err := MaybeLaunchAutoProbe(cfg)
	if err != nil {
		t.Fatalf("MaybeLaunchAutoProbe: %v", err)
	}
	if probeID != 0 {
		t.Errorf("expected no probe (within interval), got launchID=%d", probeID)
	}
}

// TestMaybeLaunchAutoProbe_NoOpWhenBreakerNotTripped: if the breaker
// isn't currently tripped, no probe is launched even if the interval
// has elapsed.
func TestMaybeLaunchAutoProbe_NoOpWhenBreakerNotTripped(t *testing.T) {
	database := db.SetupTestDB(t)
	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:           true,
			Window:            24 * time.Hour,
			AutoProbeInterval: 1 * time.Hour,
		},
	}
	probeID, err := MaybeLaunchAutoProbe(cfg)
	if err != nil {
		t.Fatalf("MaybeLaunchAutoProbe: %v", err)
	}
	if probeID != 0 {
		t.Errorf("expected no probe when breaker not tripped, got launchID=%d", probeID)
	}
}

// TestMaybeLaunchAutoProbe_DisabledByZeroInterval: setting interval=0
// turns auto-probing off entirely.
func TestMaybeLaunchAutoProbe_DisabledByZeroInterval(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind: db.EventRelaunchRunawayTripped,
		Detail:    "project=<all>; test trip",
	}); err != nil {
		t.Fatalf("trip event: %v", err)
	}
	cfg := RelaunchConfig{
		Database: database,
		RunawayPolicy: &RunawayPolicy{
			Enabled:           true,
			Window:            24 * time.Hour,
			AutoProbeInterval: 0, // disabled
		},
	}
	probeID, err := MaybeLaunchAutoProbe(cfg)
	if err != nil {
		t.Fatalf("MaybeLaunchAutoProbe: %v", err)
	}
	if probeID != 0 {
		t.Errorf("expected no probe with interval=0, got launchID=%d", probeID)
	}
}
