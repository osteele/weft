package db

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/retrypolicy"
)

func mustCreateLaunch(t *testing.T, database *sql.DB) int64 {
	t.Helper()
	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	return id
}

func TestCreateMoveIntent_Existing(t *testing.T) {
	database := SetupTestDB(t)
	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	target, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued, withLaunch(src))
	sourceAttemptID, err := GetLatestAttemptID(database, 100)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          100,
		TargetKind:     MoveTargetExisting,
		TargetLaunchID: &target,
		TargetGPUName:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if intent.State != MoveIntentStateOpen {
		t.Fatalf("state = %q, want open", intent.State)
	}
	if intent.TargetLaunchID == nil || *intent.TargetLaunchID != target {
		t.Fatalf("target_launch_id = %v, want %d", intent.TargetLaunchID, target)
	}
	if intent.SourceLaunchID == nil || *intent.SourceLaunchID != src {
		t.Fatalf("source_launch_id = %v, want %d", intent.SourceLaunchID, src)
	}
	if intent.SourceAttemptID == nil || *intent.SourceAttemptID != sourceAttemptID {
		t.Fatalf("source_attempt_id = %v, want %d", intent.SourceAttemptID, sourceAttemptID)
	}
}

func TestCreateMoveIntent_NewWithoutTargetLaunch(t *testing.T) {
	database := SetupTestDB(t)
	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued, withLaunch(src))

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:               100,
		TargetKind:          MoveTargetNew,
		TargetOfferProvider: "vastai",
		TargetOfferID:       "abc-123",
		TargetGPUName:       "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if intent.TargetLaunchID != nil {
		t.Fatalf("target_launch_id = %v, want nil for new", intent.TargetLaunchID)
	}
	if intent.TargetOfferProvider != "vastai" {
		t.Fatalf("offer provider = %q", intent.TargetOfferProvider)
	}
	if intent.MaxAttempts != retrypolicy.MaxPlacementAttempts() {
		t.Fatalf("max_attempts = %d, want shared placement budget %d", intent.MaxAttempts, retrypolicy.MaxPlacementAttempts())
	}
}

func TestCreateMoveIntent_NewIgnoresTerminalLaunchSourceAttempt(t *testing.T) {
	database := SetupTestDB(t)
	src, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed})
	if err != nil {
		t.Fatalf("CreateLaunch source: %v", err)
	}
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued, withLaunch(src))

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      100,
		TargetKind: MoveTargetNew,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if intent.SourceLaunchID != nil {
		t.Fatalf("source_launch_id = %v, want nil for terminal launch", intent.SourceLaunchID)
	}
	if intent.SourceAttemptID != nil {
		t.Fatalf("source_attempt_id = %v, want nil for terminal launch", intent.SourceAttemptID)
	}
}

func TestCreateMoveIntent_HostTarget(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 77, "echo hi", "/tmp", StatusQueued)

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      77,
		TargetKind: MoveTargetExisting,
		TargetHost: "cool30",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if intent.TargetHost != "cool30" {
		t.Fatalf("TargetHost = %q, want cool30", intent.TargetHost)
	}
	if intent.TargetLaunchID != nil {
		t.Fatalf("TargetLaunchID = %v, want nil", intent.TargetLaunchID)
	}
}

func TestCreateMoveIntent_RejectsExistingWithoutTargetLaunch(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	_, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      100,
		TargetKind: MoveTargetExisting,
	})
	if err == nil {
		t.Fatal("expected error for existing target without launch id")
	}
}

func TestCreateMoveIntent_RejectsSecondOpenIntentForSameJob(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	target := mustCreateLaunch(t, database)
	if _, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	}); err != nil {
		t.Fatalf("first CreateMoveIntent: %v", err)
	}

	_, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if !errors.Is(err, ErrMoveIntentAlreadyOpen) {
		t.Fatalf("err = %v, want ErrMoveIntentAlreadyOpen", err)
	}
}

func TestCreateMoveIntent_AllowsNewIntentAfterPriorResolved(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	target := mustCreateLaunch(t, database)
	first, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if err := ResolveMoveIntent(database, first.ID, MoveIntentStateCanceled, "test"); err != nil {
		t.Fatalf("ResolveMoveIntent: %v", err)
	}

	_, err = CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("second after resolve: %v", err)
	}
}

func TestResolveMoveIntent_SetsResolutionAndTime(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	target := mustCreateLaunch(t, database)
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := ResolveMoveIntent(database, intent.ID, MoveIntentStateConfirmed, "moved"); err != nil {
		t.Fatalf("ResolveMoveIntent: %v", err)
	}

	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != MoveIntentStateConfirmed {
		t.Fatalf("state = %q, want confirmed", got.State)
	}
	if got.Resolution != "moved" {
		t.Fatalf("resolution = %q", got.Resolution)
	}
	if got.ResolvedAt == nil {
		t.Fatal("resolved_at not set")
	}
}

func TestResolveMoveIntent_NoOpOnAlreadyResolved(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	target := mustCreateLaunch(t, database)
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if err := ResolveMoveIntent(database, intent.ID, MoveIntentStateConfirmed, "first"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	// Second resolve to a different state must not change the record.
	if err := ResolveMoveIntent(database, intent.ID, MoveIntentStateCanceled, "second"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	got, _ := GetMoveIntent(database, intent.ID)
	if got.State != MoveIntentStateConfirmed {
		t.Fatalf("state = %q, want confirmed (terminal state must not flip)", got.State)
	}
	if got.Resolution != "first" {
		t.Fatalf("resolution = %q, want %q", got.Resolution, "first")
	}
}

func TestResolveMoveIntentCanceledAbandonsHiddenTargetAttempt(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "cool30", "/tmp", "echo hi", "move")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	sourceAttemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      jobID,
		TargetKind: MoveTargetExisting,
		TargetHost: "cool100",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	targetAttemptID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "cool100", nil, StatusQueued)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if err := ResolveMoveIntent(database, intent.ID, MoveIntentStateCanceled, "superseded by explicit move"); err != nil {
		t.Fatalf("ResolveMoveIntent canceled: %v", err)
	}
	authoritative, err := GetAuthoritativeAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetAuthoritativeAttemptID: %v", err)
	}
	if authoritative != sourceAttemptID {
		t.Fatalf("authoritative = %d, want source %d", authoritative, sourceAttemptID)
	}
	var reason string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, targetAttemptID).Scan(&reason); err != nil {
		t.Fatalf("target abandoned reason: %v", err)
	}
	if reason != AttemptAbandonedMoveDestinationRejected {
		t.Fatalf("target abandoned reason = %q, want %q", reason, AttemptAbandonedMoveDestinationRejected)
	}
}

func TestResolveMoveIntentConfirmedWithTargetAttemptAbandonsSource(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "cool30", "/tmp", "echo hi", "move")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	sourceAttemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetLatestAttemptID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:      jobID,
		TargetKind: MoveTargetExisting,
		TargetHost: "cool100",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	targetAttemptID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "cool100", nil, StatusQueued)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	if err := ResolveMoveIntent(database, intent.ID, MoveIntentStateConfirmed, "accepted"); err != nil {
		t.Fatalf("ResolveMoveIntent confirmed: %v", err)
	}
	authoritative, err := GetAuthoritativeAttemptID(database, jobID)
	if err != nil {
		t.Fatalf("GetAuthoritativeAttemptID: %v", err)
	}
	if authoritative != targetAttemptID {
		t.Fatalf("authoritative = %d, want target %d", authoritative, targetAttemptID)
	}
	var reason string
	if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, sourceAttemptID).Scan(&reason); err != nil {
		t.Fatalf("source abandoned reason: %v", err)
	}
	if reason != AttemptAbandonedMoveTargetAccepted {
		t.Fatalf("source abandoned reason = %q, want %q", reason, AttemptAbandonedMoveTargetAccepted)
	}
}

func TestUpdateMoveIntentTargetLaunch(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetNew, TargetGPUName: "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	newLaunch := mustCreateLaunch(t, database)
	if err := UpdateMoveIntentTargetLaunch(database, intent.ID, newLaunch); err != nil {
		t.Fatalf("UpdateMoveIntentTargetLaunch: %v", err)
	}
	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.TargetLaunchID == nil || *got.TargetLaunchID != newLaunch {
		t.Fatalf("target_launch_id = %v, want %d", got.TargetLaunchID, newLaunch)
	}
}

func TestConfirmOpenMoveIntentsForTargetLaunch(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
	insertTestJob(t, database, 200, "echo hi", "/tmp", StatusQueued)
	insertTestJob(t, database, 300, "echo hi", "/tmp", StatusQueued)
	target := mustCreateLaunch(t, database)
	other := mustCreateLaunch(t, database)

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetNew, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent target: %v", err)
	}
	otherIntent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 200, TargetKind: MoveTargetNew, TargetLaunchID: &other,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent other: %v", err)
	}
	requestGatedIntent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 300, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent request gated: %v", err)
	}
	if err := SetMoveIntentTargetRequest(database, requestGatedIntent.ID, "jobs", "req-pending"); err != nil {
		t.Fatalf("SetMoveIntentTargetRequest: %v", err)
	}

	if err := ConfirmOpenMoveIntentsForTargetLaunch(database, target, "agent ready"); err != nil {
		t.Fatalf("ConfirmOpenMoveIntentsForTargetLaunch: %v", err)
	}
	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent target: %v", err)
	}
	if got.State != MoveIntentStateConfirmed || got.Resolution != "agent ready" {
		t.Fatalf("target intent = (%s, %q), want confirmed agent ready", got.State, got.Resolution)
	}
	gotOther, err := GetMoveIntent(database, otherIntent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent other: %v", err)
	}
	if gotOther.State != MoveIntentStateOpen {
		t.Fatalf("other intent state = %s, want open", gotOther.State)
	}
	gotRequestGated, err := GetMoveIntent(database, requestGatedIntent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent request gated: %v", err)
	}
	if gotRequestGated.State != MoveIntentStateOpen {
		t.Fatalf("request-gated intent state = %s, want open", gotRequestGated.State)
	}
}

func TestJobIDsWithOpenMoveIntents(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "j1", "/tmp", StatusQueued)
	insertTestJob(t, database, 200, "j2", "/tmp", StatusQueued)
	insertTestJob(t, database, 300, "j3", "/tmp", StatusQueued)

	target := mustCreateLaunch(t, database)
	if _, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 100, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	}); err != nil {
		t.Fatalf("create 100: %v", err)
	}
	open200, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID: 200, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
	})
	if err != nil {
		t.Fatalf("create 200: %v", err)
	}
	// Resolve 200 — should drop out of open set.
	if err := ResolveMoveIntent(database, open200.ID, MoveIntentStateConfirmed, "done"); err != nil {
		t.Fatalf("resolve 200: %v", err)
	}
	// Job 300 has no intent.

	got, err := JobIDsWithOpenMoveIntents(database)
	if err != nil {
		t.Fatalf("JobIDsWithOpenMoveIntents: %v", err)
	}
	if _, ok := got[100]; !ok {
		t.Errorf("job 100 missing from open set")
	}
	if _, ok := got[200]; ok {
		t.Errorf("job 200 should not be in open set after resolve")
	}
	if _, ok := got[300]; ok {
		t.Errorf("job 300 should not be in open set")
	}
}

func TestJobIDsWithRecentFailedMoveIntents(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "j1", "/tmp", StatusQueued)
	insertTestJob(t, database, 200, "j2", "/tmp", StatusQueued)
	insertTestJob(t, database, 300, "j3", "/tmp", StatusQueued)
	target := mustCreateLaunch(t, database)

	resolve := func(jobID int64, resolution string, resolvedAt time.Time) {
		t.Helper()
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID: jobID, TargetKind: MoveTargetExisting, TargetLaunchID: &target,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent(%d): %v", jobID, err)
		}
		if err := ResolveMoveIntent(database, intent.ID, MoveIntentStateCanceled, resolution); err != nil {
			t.Fatalf("ResolveMoveIntent(%d): %v", jobID, err)
		}
		if _, err := database.Exec(`UPDATE move_intents SET resolved_at = ? WHERE id = ?`, resolvedAt.Unix(), intent.ID); err != nil {
			t.Fatalf("set resolved_at: %v", err)
		}
	}

	now := time.Now()
	for i := 0; i < 3; i++ {
		resolve(100, "destination did not accept", now.Add(-time.Duration(i)*time.Minute))
	}
	for i := 0; i < 3; i++ {
		resolve(200, "destination did not accept", now.Add(-time.Hour))
	}
	resolve(300, "destination did not accept", now)
	resolve(300, MoveIntentResolutionStale, now)
	resolve(300, "superseded by explicit move", now)

	got, err := JobIDsWithRecentFailedMoveIntents(database, now.Add(-30*time.Minute), 3)
	if err != nil {
		t.Fatalf("JobIDsWithRecentFailedMoveIntents: %v", err)
	}
	if _, ok := got[100]; !ok {
		t.Errorf("job 100 missing from recent failed set")
	}
	if _, ok := got[200]; ok {
		t.Errorf("job 200 should be outside cooldown window")
	}
	if _, ok := got[300]; ok {
		t.Errorf("job 300 should not count benign resolutions toward failures")
	}
}

// Retry exhaustion is a real move failure and must reach the rebalance
// cooldown. Move-to-new now retries inside one intent instead of cancelling a
// fresh intent per failed launch, so a thrashing job reaches the cooldown
// threshold through exhausted intents rather than through a run of cancels.
// Both exhaustion paths must therefore stay outside the benign allowlist —
// see QueueRebalanceFailureCooldownConverges in specs/job-move.allium, which
// makes failure detection inverted precisely so a new resolution string
// cannot silently bypass the cooldown.
func TestJobIDsWithRecentFailedMoveIntents_CountsRetryExhaustion(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "j1", "/tmp", StatusQueued)
	target := mustCreateLaunch(t, database)
	now := time.Now()

	// Both strings the retry state machine writes when the budget runs out:
	// the autopilot's exhaust branch, and the DB-side restore-to-source path.
	exhaustionResolutions := []string{
		"move-to-new exhausted 5 launch attempts",
		"move target failed before start; restored to source",
	}
	for i, resolution := range exhaustionResolutions {
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID: 100, TargetKind: MoveTargetNew, TargetLaunchID: &target,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent(%d): %v", i, err)
		}
		if err := ResolveMoveIntent(database, intent.ID, MoveIntentStateCanceled, resolution); err != nil {
			t.Fatalf("ResolveMoveIntent(%d): %v", i, err)
		}
		if _, err := database.Exec(`UPDATE move_intents SET resolved_at = ? WHERE id = ?`, now.Unix(), intent.ID); err != nil {
			t.Fatalf("set resolved_at: %v", err)
		}
	}

	got, err := JobIDsWithRecentFailedMoveIntents(database, now.Add(-30*time.Minute), len(exhaustionResolutions))
	if err != nil {
		t.Fatalf("JobIDsWithRecentFailedMoveIntents: %v", err)
	}
	if _, ok := got[100]; !ok {
		t.Errorf("retry-exhaustion resolutions did not count toward the rebalance cooldown")
	}
}

func TestPruneMoveIntents(t *testing.T) {
	t.Run("stale new target without launch", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:       100,
			TargetKind:  MoveTargetNew,
			MaxAttempts: 1,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		if _, err := database.Exec(`UPDATE move_intents SET created_at = ? WHERE id = ?`, time.Now().Add(-10*time.Minute).Unix(), intent.ID); err != nil {
			t.Fatalf("age intent: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 1 || pruned[0].ID != intent.ID {
			t.Fatalf("pruned = %+v, want intent %d", pruned, intent.ID)
		}
		got, err := GetMoveIntent(database, intent.ID)
		if err != nil {
			t.Fatalf("GetMoveIntent: %v", err)
		}
		if got.State != MoveIntentStateCanceled || got.Resolution != MoveIntentResolutionStale {
			t.Fatalf("intent = (%s, %q), want canceled stale", got.State, got.Resolution)
		}
	})

	t.Run("keeps retryable stale new target without launch", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:       100,
			TargetKind:  MoveTargetNew,
			MaxAttempts: 5,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		if _, err := database.Exec(`UPDATE move_intents SET created_at = ? WHERE id = ?`, time.Now().Add(-10*time.Minute).Unix(), intent.ID); err != nil {
			t.Fatalf("age intent: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 0 {
			t.Fatalf("pruned = %+v, want none", pruned)
		}
	})

	t.Run("reaps old retryable stale new target without launch", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:        100,
			TargetKind:   MoveTargetNew,
			AttemptCount: 1,
			MaxAttempts:  5,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		if _, err := database.Exec(
			`UPDATE move_intents SET created_at = ? WHERE id = ?`,
			time.Now().Add(-MoveIntentRetryPruneAgeLimit-time.Minute).Unix(), intent.ID,
		); err != nil {
			t.Fatalf("age intent: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 1 || pruned[0].ID != intent.ID {
			t.Fatalf("pruned = %+v, want intent %d", pruned, intent.ID)
		}
		got, err := GetMoveIntent(database, intent.ID)
		if err != nil {
			t.Fatalf("GetMoveIntent: %v", err)
		}
		if got.State != MoveIntentStateCanceled || got.Resolution != MoveIntentResolutionStale {
			t.Fatalf("intent = (%s, %q), want canceled stale", got.State, got.Resolution)
		}
	})

	t.Run("keeps fresh unlaunched move", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:      100,
			TargetKind: MoveTargetNew,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 0 {
			t.Fatalf("pruned = %+v, want none", pruned)
		}
		got, err := GetMoveIntent(database, intent.ID)
		if err != nil {
			t.Fatalf("GetMoveIntent: %v", err)
		}
		if got.State != MoveIntentStateOpen {
			t.Fatalf("state = %s, want open", got.State)
		}
	})

	t.Run("keeps live target launch", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
		target := mustCreateLaunch(t, database)
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:          100,
			TargetKind:     MoveTargetNew,
			TargetLaunchID: &target,
			MaxAttempts:    1,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		if _, err := database.Exec(`UPDATE move_intents SET created_at = ? WHERE id = ?`, time.Now().Add(-10*time.Minute).Unix(), intent.ID); err != nil {
			t.Fatalf("age intent: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 0 {
			t.Fatalf("pruned = %+v, want none", pruned)
		}
	})

	t.Run("keeps retryable terminal target launch", func(t *testing.T) {
		database := SetupTestDB(t)
		insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)
		target, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:          100,
			TargetKind:     MoveTargetNew,
			TargetLaunchID: &target,
			MaxAttempts:    5,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		if _, err := database.Exec(`UPDATE move_intents SET created_at = ? WHERE id = ?`, time.Now().Add(-10*time.Minute).Unix(), intent.ID); err != nil {
			t.Fatalf("age intent: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 0 {
			t.Fatalf("pruned = %+v, want none", pruned)
		}
	})

	t.Run("abandon hidden target and converge", func(t *testing.T) {
		database := SetupTestDB(t)
		jobID, err := RecordQueued(database, "cool30", "/tmp", "echo hi", "move")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		sourceAttemptID, err := GetLatestAttemptID(database, jobID)
		if err != nil {
			t.Fatalf("GetLatestAttemptID source: %v", err)
		}
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:      jobID,
			TargetKind: MoveTargetExisting,
			TargetHost: "cool100",
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		if intent.SourceAttemptID == nil || *intent.SourceAttemptID != sourceAttemptID {
			t.Fatalf("source_attempt_id = %v, want %d", intent.SourceAttemptID, sourceAttemptID)
		}
		targetAttemptID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "cool100", nil, StatusQueued)
		if err != nil {
			t.Fatalf("CreateMoveTargetAttempt: %v", err)
		}
		if _, err := database.Exec(`UPDATE move_intents SET created_at = ? WHERE id = ?`, time.Now().Add(-10*time.Minute).Unix(), intent.ID); err != nil {
			t.Fatalf("age intent: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 1 || pruned[0].ID != intent.ID {
			t.Fatalf("pruned = %+v, want intent %d", pruned, intent.ID)
		}
		var reason string
		if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, targetAttemptID).Scan(&reason); err != nil {
			t.Fatalf("target abandoned reason: %v", err)
		}
		if reason != AttemptAbandonedMoveDestinationRejected {
			t.Fatalf("target abandoned reason = %q, want %q", reason, AttemptAbandonedMoveDestinationRejected)
		}

		pruned, err = PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents second pass: %v", err)
		}
		if len(pruned) != 0 {
			t.Fatalf("second prune = %+v, want none", pruned)
		}
	})

	t.Run("repairs missing source snapshot after source finished", func(t *testing.T) {
		database := SetupTestDB(t)
		src := mustCreateLaunch(t, database)
		target := mustCreateLaunch(t, database)
		jobID, err := RecordQueued(database, "", "/tmp", "echo hi", "move")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := SetJobLaunchID(database, jobID, src); err != nil {
			t.Fatalf("SetJobLaunchID source: %v", err)
		}
		sourceAttemptID, err := GetLatestAttemptID(database, jobID)
		if err != nil {
			t.Fatalf("GetLatestAttemptID source: %v", err)
		}
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:          jobID,
			TargetKind:     MoveTargetExisting,
			TargetLaunchID: &target,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		targetAttemptID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "", &target, StatusQueued)
		if err != nil {
			t.Fatalf("CreateMoveTargetAttempt: %v", err)
		}
		now := time.Now().Unix()
		if _, err := database.Exec(`UPDATE move_intents SET source_attempt_id = NULL WHERE id = ?`, intent.ID); err != nil {
			t.Fatalf("clear source snapshot: %v", err)
		}
		if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ?, exit_code = 0 WHERE id = ?`, StatusCompleted, now, sourceAttemptID); err != nil {
			t.Fatalf("finish source attempt: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 1 || pruned[0].ID != intent.ID {
			t.Fatalf("pruned = %+v, want intent %d", pruned, intent.ID)
		}
		if pruned[0].State != MoveIntentStateObsoleted {
			t.Fatalf("pruned state = %s, want obsoleted", pruned[0].State)
		}
		got, err := GetMoveIntent(database, intent.ID)
		if err != nil {
			t.Fatalf("GetMoveIntent: %v", err)
		}
		if got.State != MoveIntentStateObsoleted {
			t.Fatalf("state = %s, want obsoleted", got.State)
		}
		var reason string
		if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, targetAttemptID).Scan(&reason); err != nil {
			t.Fatalf("target abandoned reason: %v", err)
		}
		if reason != AttemptAbandonedMoveSourceWon {
			t.Fatalf("target abandoned reason = %q, want %q", reason, AttemptAbandonedMoveSourceWon)
		}
	})

	t.Run("repairs canceled target attempt as canceled", func(t *testing.T) {
		// wj4620: a launch reset canceled both the source attempt and the
		// hidden target attempt but left the intent open. A canceled target
		// never won, so the repair must resolve canceled — not confirmed.
		database := SetupTestDB(t)
		src := mustCreateLaunch(t, database)
		target := mustCreateLaunch(t, database)
		jobID, err := RecordQueued(database, "", "/tmp", "echo hi", "move")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := SetJobLaunchID(database, jobID, src); err != nil {
			t.Fatalf("SetJobLaunchID source: %v", err)
		}
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:          jobID,
			TargetKind:     MoveTargetExisting,
			TargetLaunchID: &target,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		targetAttemptID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "", &target, StatusQueued)
		if err != nil {
			t.Fatalf("CreateMoveTargetAttempt: %v", err)
		}
		now := time.Now().Unix()
		if _, err := database.Exec(
			`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ?`,
			StatusCanceled, now, jobID,
		); err != nil {
			t.Fatalf("cancel attempts: %v", err)
		}
		if _, err := database.Exec(`UPDATE move_intents SET source_attempt_id = NULL WHERE id = ?`, intent.ID); err != nil {
			t.Fatalf("clear source snapshot: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 1 || pruned[0].ID != intent.ID {
			t.Fatalf("pruned = %+v, want intent %d", pruned, intent.ID)
		}
		if pruned[0].State != MoveIntentStateCanceled {
			t.Fatalf("pruned state = %s, want canceled", pruned[0].State)
		}
		got, err := GetMoveIntent(database, intent.ID)
		if err != nil {
			t.Fatalf("GetMoveIntent: %v", err)
		}
		if got.State != MoveIntentStateCanceled {
			t.Fatalf("state = %s, want canceled", got.State)
		}
		if !strings.Contains(got.Resolution, "target attempt canceled") {
			t.Fatalf("resolution = %q, want it to mention target attempt canceled", got.Resolution)
		}
		var abandonedReason string
		if err := database.QueryRow(
			`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, targetAttemptID,
		).Scan(&abandonedReason); err != nil {
			t.Fatalf("target abandoned reason: %v", err)
		}
		if abandonedReason != AttemptAbandonedMoveDestinationRejected {
			t.Fatalf("target abandoned reason = %q, want %q", abandonedReason, AttemptAbandonedMoveDestinationRejected)
		}
	})

	t.Run("repairs completed target attempt", func(t *testing.T) {
		database := SetupTestDB(t)
		target := mustCreateLaunch(t, database)
		jobID, err := RecordQueued(database, "cool30", "/tmp", "echo hi", "move")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		sourceAttemptID, err := GetLatestAttemptID(database, jobID)
		if err != nil {
			t.Fatalf("GetLatestAttemptID source: %v", err)
		}
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:          jobID,
			TargetKind:     MoveTargetExisting,
			TargetLaunchID: &target,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent: %v", err)
		}
		now := time.Now().Unix()
		res, err := database.Exec(
			`INSERT INTO job_attempts (job_id, attempt_number, launch_id, status, queued_at, start_time, end_time, exit_code, move_intent_id)
			 VALUES (?, 2, ?, ?, ?, ?, ?, 0, ?)`,
			jobID, target, StatusCompleted, now, now, now, intent.ID,
		)
		if err != nil {
			t.Fatalf("insert completed target attempt: %v", err)
		}
		targetAttemptID, _ := res.LastInsertId()
		if _, err := database.Exec(`UPDATE move_intents SET target_attempt_id = ? WHERE id = ?`, targetAttemptID, intent.ID); err != nil {
			t.Fatalf("link target attempt: %v", err)
		}

		pruned, err := PruneMoveIntents(database, 5*time.Minute)
		if err != nil {
			t.Fatalf("PruneMoveIntents: %v", err)
		}
		if len(pruned) != 1 || pruned[0].ID != intent.ID {
			t.Fatalf("pruned = %+v, want intent %d", pruned, intent.ID)
		}
		if pruned[0].State != MoveIntentStateConfirmed {
			t.Fatalf("pruned state = %s, want confirmed", pruned[0].State)
		}
		got, err := GetMoveIntent(database, intent.ID)
		if err != nil {
			t.Fatalf("GetMoveIntent: %v", err)
		}
		if got.State != MoveIntentStateConfirmed {
			t.Fatalf("state = %s, want confirmed", got.State)
		}
		var reason string
		if err := database.QueryRow(`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, sourceAttemptID).Scan(&reason); err != nil {
			t.Fatalf("source abandoned reason: %v", err)
		}
		if reason != AttemptAbandonedMoveTargetAccepted {
			t.Fatalf("source abandoned reason = %q, want %q", reason, AttemptAbandonedMoveTargetAccepted)
		}
	})
}

func TestPruneMoveIntents_PreservesRetryableMoveToNewTargetFailure(t *testing.T) {
	database := SetupTestDB(t)
	source := mustCreateLaunch(t, database)
	target, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
	if err != nil {
		t.Fatalf("CreateLaunch target: %v", err)
	}
	jobID, err := RecordQueued(database, "", t.TempDir(), "python train.py", "move")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, source); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          jobID,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &target,
		AttemptCount:   1,
		MaxAttempts:    5,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	if _, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "", &target, StatusQueued); err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	transition, err := HandleMoveTargetFailedBeforeStart(database, jobID, target, AttemptOutcomeOrphaned)
	if err != nil {
		t.Fatalf("HandleMoveTargetFailedBeforeStart: %v", err)
	}
	if !transition.Handled || !transition.Retryable || transition.Exhausted {
		t.Fatalf("transition = %+v, want handled retryable not exhausted", transition)
	}

	pruned, err := PruneMoveIntents(database, 5*time.Minute)
	if err != nil {
		t.Fatalf("PruneMoveIntents: %v", err)
	}
	if len(pruned) != 0 {
		t.Fatalf("pruned retryable intent = %+v, want none", pruned)
	}
	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != MoveIntentStateOpen {
		t.Fatalf("state = %s, want open", got.State)
	}
	if got.AttemptCount != 1 || got.MaxAttempts != 5 {
		t.Fatalf("attempt budget = %d/%d, want 1/5", got.AttemptCount, got.MaxAttempts)
	}
}

func TestPruneMoveIntents_ReapsMoveToNewRetryAfterBound(t *testing.T) {
	database := SetupTestDB(t)
	source := mustCreateLaunch(t, database)
	target := mustCreateLaunch(t, database)
	jobID, err := RecordQueued(database, "", t.TempDir(), "python train.py", "move")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, source); err != nil {
		t.Fatalf("SetJobLaunchID source: %v", err)
	}
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:          jobID,
		TargetKind:     MoveTargetNew,
		TargetLaunchID: &target,
		AttemptCount:   1,
		MaxAttempts:    5,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	targetAttemptID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "", &target, StatusQueued)
	if err != nil {
		t.Fatalf("CreateMoveTargetAttempt: %v", err)
	}
	now := time.Now().Unix()
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE id = ?`, StatusCanceled, now, targetAttemptID); err != nil {
		t.Fatalf("cancel target attempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE move_intents SET created_at = ? WHERE id = ?`, time.Now().Add(-2*time.Hour-time.Minute).Unix(), intent.ID); err != nil {
		t.Fatalf("age intent: %v", err)
	}

	pruned, err := PruneMoveIntents(database, 5*time.Minute)
	if err != nil {
		t.Fatalf("PruneMoveIntents: %v", err)
	}
	if len(pruned) != 1 || pruned[0].ID != intent.ID {
		t.Fatalf("pruned = %+v, want intent %d", pruned, intent.ID)
	}
	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent: %v", err)
	}
	if got.State != MoveIntentStateCanceled {
		t.Fatalf("state = %s, want canceled", got.State)
	}
}

func TestRequeue_AbandonsOpenMoveIntent(t *testing.T) {
	// Every full-requeue path closes all of the job's attempts, so each must
	// resolve the open move intent, or the requeued job stays hidden from the
	// autopilot (AutopilotIgnoresMovingJobs). See RequeueAbandonsOpenMoveIntent
	// in specs/job-move.allium.
	cases := []struct {
		name    string
		requeue func(database *sql.DB, jobID int64) error
	}{
		// Unplace, replan, and restart's unplaced branch.
		{"ResetJobToUnplaced", ResetJobToUnplaced},
		// User requeue; the latest (move-target) attempt has a launch, so
		// this routes through RequeueFreshAttemptByTargetTx.
		{"RequeueByID", RequeueByID},
		// RequeueByID's on-prem branch.
		{"RequeueByIDTx", func(database *sql.DB, jobID int64) error {
			tx, err := database.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if err := RequeueByIDTx(tx, jobID); err != nil {
				return err
			}
			return tx.Commit()
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := SetupTestDB(t)
			src := mustCreateLaunch(t, database)
			target := mustCreateLaunch(t, database)
			jobID, err := RecordQueued(database, "", "/tmp", "echo hi", "move")
			if err != nil {
				t.Fatalf("RecordQueued: %v", err)
			}
			if err := SetJobLaunchID(database, jobID, src); err != nil {
				t.Fatalf("SetJobLaunchID: %v", err)
			}
			intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
				JobID:          jobID,
				TargetKind:     MoveTargetExisting,
				TargetLaunchID: &target,
			})
			if err != nil {
				t.Fatalf("CreateMoveIntent: %v", err)
			}
			targetAttemptID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "", &target, StatusQueued)
			if err != nil {
				t.Fatalf("CreateMoveTargetAttempt: %v", err)
			}

			if err := tc.requeue(database, jobID); err != nil {
				t.Fatalf("requeue: %v", err)
			}

			got, err := GetMoveIntent(database, intent.ID)
			if err != nil {
				t.Fatalf("GetMoveIntent: %v", err)
			}
			if got.State != MoveIntentStateCanceled {
				t.Fatalf("intent state = %s, want canceled", got.State)
			}
			if !strings.Contains(got.Resolution, "move abandoned") {
				t.Fatalf("resolution = %q, want it to mention move abandoned", got.Resolution)
			}
			var abandonedReason string
			if err := database.QueryRow(
				`SELECT COALESCE(abandoned_reason, '') FROM job_attempts WHERE id = ?`, targetAttemptID,
			).Scan(&abandonedReason); err != nil {
				t.Fatalf("target abandoned reason: %v", err)
			}
			if abandonedReason != AttemptAbandonedMoveDestinationRejected {
				t.Fatalf("target abandoned reason = %q, want %q", abandonedReason, AttemptAbandonedMoveDestinationRejected)
			}
		})
	}
}

func TestListAbandonedMoveTargetCancelAttempts(t *testing.T) {
	database := SetupTestDB(t)
	src := mustCreateLaunch(t, database)
	target := mustCreateLaunch(t, database)

	openIntent := func(jobID int64) (*MoveIntent, int64) {
		t.Helper()
		if err := SetJobLaunchID(database, jobID, src); err != nil {
			t.Fatalf("SetJobLaunchID job %d: %v", jobID, err)
		}
		intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
			JobID:          jobID,
			TargetKind:     MoveTargetExisting,
			TargetLaunchID: &target,
		})
		if err != nil {
			t.Fatalf("CreateMoveIntent job %d: %v", jobID, err)
		}
		attemptID, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "", &target, StatusQueued)
		if err != nil {
			t.Fatalf("CreateMoveTargetAttempt job %d: %v", jobID, err)
		}
		return intent, attemptID
	}

	// Abandoned before start: should be listed.
	insertTestJob(t, database, 300, "echo hi", "/tmp", StatusQueued)
	intentA, attemptA := openIntent(300)
	if err := ResolveMoveIntent(database, intentA.ID, MoveIntentStateCanceled, "launch reset; move abandoned"); err != nil {
		t.Fatalf("resolve intent A: %v", err)
	}

	// Still open: not listed.
	insertTestJob(t, database, 301, "echo hi", "/tmp", StatusQueued)
	_, attemptB := openIntent(301)

	// Abandoned but the target attempt had already started: not listed.
	insertTestJob(t, database, 302, "echo hi", "/tmp", StatusQueued)
	intentC, attemptC := openIntent(302)
	if _, err := database.Exec(`UPDATE job_attempts SET start_time = ? WHERE id = ?`, time.Now().Unix(), attemptC); err != nil {
		t.Fatalf("start attempt C: %v", err)
	}
	if err := ResolveMoveIntent(database, intentC.ID, MoveIntentStateCanceled, "canceled after start"); err != nil {
		t.Fatalf("resolve intent C: %v", err)
	}

	got, err := ListAbandonedMoveTargetCancelAttempts(database, target, time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatalf("ListAbandonedMoveTargetCancelAttempts: %v", err)
	}
	if len(got) != 1 || got[0] != attemptA {
		t.Fatalf("attempts = %v, want [%d] (not open %d or started %d)", got, attemptA, attemptB, attemptC)
	}

	// Outside the recency window: not listed.
	got, err = ListAbandonedMoveTargetCancelAttempts(database, target, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("ListAbandonedMoveTargetCancelAttempts future window: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("attempts = %v, want none outside window", got)
	}
}

func TestAttachMoveIntentTargetLaunch(t *testing.T) {
	database := SetupTestDB(t)
	insertTestJob(t, database, 100, "echo hi", "/tmp", StatusQueued)

	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{
		JobID:       100,
		TargetKind:  MoveTargetNew,
		MaxAttempts: 4,
	})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	first := mustCreateLaunch(t, database)
	if err := AttachMoveIntentTargetLaunch(database, intent, first); err != nil {
		t.Fatalf("AttachMoveIntentTargetLaunch first: %v", err)
	}
	got, err := GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent first: %v", err)
	}
	if got.TargetLaunchID == nil || *got.TargetLaunchID != first {
		t.Fatalf("target_launch_id = %v, want %d", got.TargetLaunchID, first)
	}
	if got.AttemptCount != 1 {
		t.Fatalf("attempt_count after first attach = %d, want 1", got.AttemptCount)
	}

	second := mustCreateLaunch(t, database)
	if err := AttachMoveIntentTargetLaunch(database, got, second); err != nil {
		t.Fatalf("AttachMoveIntentTargetLaunch second: %v", err)
	}
	got, err = GetMoveIntent(database, intent.ID)
	if err != nil {
		t.Fatalf("GetMoveIntent second: %v", err)
	}
	if got.TargetLaunchID == nil || *got.TargetLaunchID != second {
		t.Fatalf("target_launch_id = %v, want %d", got.TargetLaunchID, second)
	}
	if got.AttemptCount != 2 {
		t.Fatalf("attempt_count after replacement attach = %d, want 2", got.AttemptCount)
	}
}
