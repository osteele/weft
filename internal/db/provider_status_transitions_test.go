package db

import (
	"testing"
	"time"
)

// TestRecordProviderStatus_RecordsFirstObservation pins the empty→non-empty
// edge the empty-status watchdog is measured against. Without this row that
// watchdog's threshold cannot be derived from history, and it cannot derive it
// itself: it terminates instances before they produce the observation.
func TestRecordProviderStatus_RecordsFirstObservation(t *testing.T) {
	database := setupTestDB(t)
	launchID := createTestLaunch(t, database, LaunchStatusLaunching)

	first := time.Now().Add(-3 * time.Minute)
	if err := RecordProviderStatus(database, launchID, first, "", "loading"); err != nil {
		t.Fatalf("RecordProviderStatus: %v", err)
	}

	transitions, err := GetProviderStatusTransitions(database, launchID)
	if err != nil {
		t.Fatalf("GetProviderStatusTransitions: %v", err)
	}
	if len(transitions) != 1 {
		t.Fatalf("got %d transitions, want the first observation to be recorded", len(transitions))
	}
	if transitions[0].OldStatus != "" || transitions[0].NewStatus != "loading" {
		t.Errorf("transition = %q→%q, want \"\"→\"loading\"", transitions[0].OldStatus, transitions[0].NewStatus)
	}
	if transitions[0].ObservedAt != first.Unix() {
		t.Errorf("ObservedAt = %d, want %d — the timestamp is the measurement", transitions[0].ObservedAt, first.Unix())
	}
}

// TestLastProviderStatusTransitionTime_IgnoresFirstObservation: the
// stale-status watchdog anchors on this, so it must mean "last change". A
// first observation says the provider became legible, not that anything
// changed; anchoring on it would push that deadline out by the provider's
// reporting latency on every launch.
func TestLastProviderStatusTransitionTime_IgnoresFirstObservation(t *testing.T) {
	database := setupTestDB(t)
	launchID := createTestLaunch(t, database, LaunchStatusLaunching)

	first := time.Now().Add(-5 * time.Minute)
	if err := RecordProviderStatus(database, launchID, first, "", "loading"); err != nil {
		t.Fatalf("RecordProviderStatus: %v", err)
	}

	got, err := LastProviderStatusTransitionTime(database, launchID)
	if err != nil {
		t.Fatalf("LastProviderStatusTransitionTime: %v", err)
	}
	if got != nil {
		t.Fatalf("got %v, want nil — a first observation is not a status change", got)
	}
}

// TestRecordProviderStatus_FirstObservationYieldsMeasurableLatency is the
// point of the whole change: with the first observation recorded, the interval
// the empty-status watchdog governs becomes derivable from stored data. Both
// ends of the interval are read back out of the database so the test measures
// what a consumer would measure, not its own local variables.
func TestRecordProviderStatus_FirstObservationYieldsMeasurableLatency(t *testing.T) {
	database := setupTestDB(t)
	launchID := createTestLaunch(t, database, LaunchStatusLaunching)

	// Second granularity: observed_at is stored as a Unix second count.
	launchedAt := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	if _, err := database.Exec(`UPDATE launches SET launched_at = ? WHERE id = ?`, launchedAt.Unix(), launchID); err != nil {
		t.Fatalf("set launched_at: %v", err)
	}
	if err := RecordProviderStatus(database, launchID, launchedAt.Add(4*time.Minute), "", "loading"); err != nil {
		t.Fatalf("RecordProviderStatus: %v", err)
	}

	launch, err := GetLaunch(database, launchID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch == nil || launch.LaunchedAt == nil {
		t.Fatal("launch has no stored launched_at")
	}
	transitions, err := GetProviderStatusTransitions(database, launchID)
	if err != nil {
		t.Fatalf("GetProviderStatusTransitions: %v", err)
	}
	if len(transitions) != 1 {
		t.Fatalf("got %d transitions, want the first observation to be recorded", len(transitions))
	}
	if transitions[0].OldStatus != "" {
		t.Fatalf("OldStatus = %q, want the empty first-observation marker", transitions[0].OldStatus)
	}

	latency := time.Duration(transitions[0].ObservedAt-*launch.LaunchedAt) * time.Second
	if latency != 4*time.Minute {
		t.Errorf("derived empty-status latency = %s, want 4m0s", latency)
	}
}

// TestRecordProviderStatus_RepeatedFirstObservationsCollapse is the regression
// that made this whole change safe to ship.
//
// Several paths construct a Reconciler per poll — cmd/status.go builds one
// inside a 3-second ticker — so the caller's in-memory "previous status" is
// empty on every tick. Taking that at face value would append a row per tick
// for every running launch, and a row written hours into a launch would read
// as hours of provider reporting latency, corrupting the measurement this
// change exists to enable.
func TestRecordProviderStatus_RepeatedFirstObservationsCollapse(t *testing.T) {
	database := setupTestDB(t)
	launchID := createTestLaunch(t, database, LaunchStatusLaunching)

	first := time.Now().Add(-30 * time.Minute)
	for i := range 10 {
		// Every call reports an empty oldStatus, as a freshly built
		// reconciler does.
		at := first.Add(time.Duration(i) * 3 * time.Second)
		if err := RecordProviderStatus(database, launchID, at, "", "running"); err != nil {
			t.Fatalf("RecordProviderStatus %d: %v", i, err)
		}
	}

	transitions, err := GetProviderStatusTransitions(database, launchID)
	if err != nil {
		t.Fatalf("GetProviderStatusTransitions: %v", err)
	}
	if len(transitions) != 1 {
		t.Fatalf("got %d transitions from 10 identical observations, want 1", len(transitions))
	}
	if transitions[0].ObservedAt != first.Unix() {
		t.Errorf("kept the row at %d, want the earliest at %d — a later one would overstate the latency", transitions[0].ObservedAt, first.Unix())
	}
}

// TestRecordProviderStatus_RecoversPredecessorAcrossObserverRestart: a caller
// that lost its memory must not lose the transition. The table outlives the
// observer, so a genuine change still lands with its real predecessor rather
// than as a second first-observation.
func TestRecordProviderStatus_RecoversPredecessorAcrossObserverRestart(t *testing.T) {
	database := setupTestDB(t)
	launchID := createTestLaunch(t, database, LaunchStatusLaunching)

	start := time.Now().Add(-20 * time.Minute)
	if err := RecordProviderStatus(database, launchID, start, "", "running"); err != nil {
		t.Fatalf("first observation: %v", err)
	}
	// Observer restarts, so it reports "" again — but the status has changed.
	if err := RecordProviderStatus(database, launchID, start.Add(5*time.Minute), "", "exited"); err != nil {
		t.Fatalf("post-restart observation: %v", err)
	}

	transitions, err := GetProviderStatusTransitions(database, launchID)
	if err != nil {
		t.Fatalf("GetProviderStatusTransitions: %v", err)
	}
	if len(transitions) != 2 {
		t.Fatalf("got %d transitions, want 2", len(transitions))
	}
	if transitions[1].OldStatus != "running" || transitions[1].NewStatus != "exited" {
		t.Errorf("second transition = %q→%q, want \"running\"→\"exited\" — the predecessor comes from the table, not the caller", transitions[1].OldStatus, transitions[1].NewStatus)
	}
}
