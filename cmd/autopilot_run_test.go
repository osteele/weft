package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/orchestration"
)

func TestClassifyAutopilotPass(t *testing.T) {
	pausedWait := 42 * time.Second

	tests := []struct {
		name        string
		result      *orchestration.GroupedAutoPilotResult
		err         error
		wantOutcome autopilotOutcome
		wantWait    time.Duration
	}{
		{
			name:        "paused",
			err:         orchestration.ErrAutopilotPaused,
			wantOutcome: outcomePaused,
			wantWait:    pausedWait,
		},
		{
			name:        "busy",
			err:         orchestration.ErrAutopilotBusy,
			wantOutcome: outcomeBusy,
			wantWait:    orchestration.AutopilotCooldownContend,
		},
		{
			name:        "error",
			err:         errors.New("boom"),
			wantOutcome: outcomeError,
			wantWait:    orchestration.AutopilotCooldownError,
		},
		{
			name:        "nil result is idle",
			result:      nil,
			wantOutcome: outcomeIdle,
			wantWait:    orchestration.AutopilotCooldownIdle,
		},
		{
			name:        "placed counts as progress",
			result:      &orchestration.GroupedAutoPilotResult{Placed: 1},
			wantOutcome: outcomeProgress,
			wantWait:    orchestration.AutopilotCooldownProgress,
		},
		{
			name:        "launched counts as progress",
			result:      &orchestration.GroupedAutoPilotResult{Launched: 1},
			wantOutcome: outcomeProgress,
			wantWait:    orchestration.AutopilotCooldownProgress,
		},
		{
			name:        "rebalanced counts as progress",
			result:      &orchestration.GroupedAutoPilotResult{Rebalanced: 1},
			wantOutcome: outcomeProgress,
			wantWait:    orchestration.AutopilotCooldownProgress,
		},
		{
			name:        "blocked-only with no progress",
			result:      &orchestration.GroupedAutoPilotResult{BlockedReasons: map[int64]string{1: "no offer"}},
			wantOutcome: outcomeBlocked,
			wantWait:    orchestration.AutopilotCooldownBlocked,
		},
		{
			name:        "progress wins over blocked",
			result:      &orchestration.GroupedAutoPilotResult{Launched: 1, BlockedReasons: map[int64]string{1: "no offer"}},
			wantOutcome: outcomeProgress,
			wantWait:    orchestration.AutopilotCooldownProgress,
		},
		{
			name:        "empty result is idle",
			result:      &orchestration.GroupedAutoPilotResult{},
			wantOutcome: outcomeIdle,
			wantWait:    orchestration.AutopilotCooldownIdle,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotOutcome, gotWait := classifyAutopilotPass(tc.result, tc.err, pausedWait)
			if gotOutcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", gotOutcome, tc.wantOutcome)
			}
			if gotWait != tc.wantWait {
				t.Errorf("wait = %s, want %s", gotWait, tc.wantWait)
			}
		})
	}
}

func TestAnyBlockedReason(t *testing.T) {
	if got := anyBlockedReason(nil); got != "" {
		t.Errorf("nil map: got %q, want empty", got)
	}
	if got := anyBlockedReason(map[int64]string{1: "  ", 2: ""}); got != "" {
		t.Errorf("blank reasons: got %q, want empty", got)
	}
	if got := anyBlockedReason(map[int64]string{1: " hello "}); got != "hello" {
		t.Errorf("trimmed: got %q, want %q", got, "hello")
	}
}

func TestAutopilotTimerRunsPass(t *testing.T) {
	tests := []struct {
		outcome autopilotOutcome
		want    bool
	}{
		{outcomeProgress, true},
		{outcomeBlocked, true},
		{outcomeError, true},
		{outcomeBusy, true},
		{outcomePaused, true},
		{outcomeIdle, false},
	}
	for _, tc := range tests {
		if got := autopilotTimerRunsPass(tc.outcome); got != tc.want {
			t.Errorf("autopilotTimerRunsPass(%q) = %v, want %v", tc.outcome, got, tc.want)
		}
	}
}

func TestReadAutopilotWakeSnapshot(t *testing.T) {
	database := db.SetupTestDB(t)

	initial, err := readAutopilotWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read initial snapshot: %v", err)
	}

	if _, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "unplaced", "A100"); err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	afterJob, err := readAutopilotWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read job snapshot: %v", err)
	}
	if afterJob.UnplacedJobs != initial.UnplacedJobs+1 {
		t.Fatalf("UnplacedJobs = %d, want %d", afterJob.UnplacedJobs, initial.UnplacedJobs+1)
	}

	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{EventKind: db.EventRetryNoOffers}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}
	afterLifecycle, err := readAutopilotWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read lifecycle snapshot: %v", err)
	}
	if afterLifecycle.LifecycleID <= afterJob.LifecycleID {
		t.Fatalf("LifecycleID = %d, want > %d", afterLifecycle.LifecycleID, afterJob.LifecycleID)
	}

	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	afterLaunch, err := readAutopilotWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read launch snapshot: %v", err)
	}
	if afterLaunch.LiveLaunches != afterLifecycle.LiveLaunches+1 {
		t.Fatalf("LiveLaunches = %d, want %d", afterLaunch.LiveLaunches, afterLifecycle.LiveLaunches+1)
	}

	placementJob, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python place.py", "placing", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU placement: %v", err)
	}
	if _, err := db.CreatePlacementIntent(database, placementJob, "test"); err != nil {
		t.Fatalf("CreatePlacementIntent: %v", err)
	}
	moveJob, err := db.RecordQueuedWithGPU(database, db.LaunchHost(launchID), t.TempDir(), "python move.py", "moving", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU move: %v", err)
	}
	if _, err := db.CreateMoveIntent(database, db.CreateMoveIntentParams{JobID: moveJob, TargetKind: db.MoveTargetNew}); err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	afterIntents, err := readAutopilotWakeSnapshot(database)
	if err != nil {
		t.Fatalf("read intent snapshot: %v", err)
	}
	if afterIntents.OpenPlacementIntents != afterLaunch.OpenPlacementIntents+1 {
		t.Fatalf("OpenPlacementIntents = %d, want %d", afterIntents.OpenPlacementIntents, afterLaunch.OpenPlacementIntents+1)
	}
	if afterIntents.OpenMoveIntents != afterLaunch.OpenMoveIntents+1 {
		t.Fatalf("OpenMoveIntents = %d, want %d", afterIntents.OpenMoveIntents, afterLaunch.OpenMoveIntents+1)
	}
}
