package campaign

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
)

func TestCheckInstance_GraceExpired_ForceDestroyAfterTimeout(t *testing.T) {
	// Grace expired well past the shutdown timeout — no termination intent from agent.
	// Coordinator should force-destroy.
	pastDeadline := time.Now().Add(-5 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:            1,
			Status:        db.LaunchStatusGrace,
			GraceDeadline: &pastDeadline,
		},
		Now: time.Now(),
	})
	if action.Kind != ActionGraceExpired {
		t.Fatalf("action.Kind = %d, want ActionGraceExpired (%d)", action.Kind, ActionGraceExpired)
	}
	if action.TerminalStatus != db.LaunchStatusFailed {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusFailed)
	}
	if !action.DestroyProvider {
		t.Error("DestroyProvider should be true")
	}
	if action.ResetJobs {
		t.Error("ResetJobs should be false for grace expiry (job already ran and failed)")
	}
	if action.AttemptOutcome != db.AttemptOutcomeFailed {
		t.Errorf("AttemptOutcome = %q, want %q", action.AttemptOutcome, db.AttemptOutcomeFailed)
	}
}

func TestCheckInstance_GraceExpired_WaitForAgentShutdown(t *testing.T) {
	// Grace expired recently (within shutdown timeout) and no termination intent.
	// Coordinator should wait, not destroy.
	recentDeadline := time.Now().Add(-30 * time.Second).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:            1,
			Status:        db.LaunchStatusGrace,
			GraceDeadline: &recentDeadline,
		},
		Now: time.Now(),
	})
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %d, want ActionDisplayOnly (%d)", action.Kind, ActionDisplayOnly)
	}
	if action.DestroyProvider {
		t.Error("DestroyProvider should be false while waiting for agent shutdown")
	}
	if action.StallMessage == "" {
		t.Error("expected a stall message")
	}
}

func TestCheckInstance_GraceExpired_AgentIntentPresent(t *testing.T) {
	// Grace expired and agent has written a termination intent.
	// Step 1 should fall through to step 3 (termination intent handler).
	pastDeadline := time.Now().Add(-5 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:            1,
			Status:        db.LaunchStatusGrace,
			GraceDeadline: &pastDeadline,
		},
		TerminationIntent: &instanceintent.Marker{
			TerminalStatus:    db.LaunchStatusFailed,
			TerminationReason: db.TerminationReasonJobFailure,
		},
		Now: time.Now(),
	})
	if action.Kind != ActionTerminationIntent {
		t.Fatalf("action.Kind = %d, want ActionTerminationIntent (%d); grace expiry should defer to termination intent", action.Kind, ActionTerminationIntent)
	}
	if action.TerminalStatus != db.LaunchStatusFailed {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusFailed)
	}
}

func TestCheckInstance_GraceNotExpired(t *testing.T) {
	futureDeadline := time.Now().Add(5 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:            1,
			Status:        db.LaunchStatusGrace,
			GraceDeadline: &futureDeadline,
		},
		Now: time.Now(),
	})
	if action.Kind != ActionNone {
		t.Fatalf("action.Kind = %d, want ActionNone (%d)", action.Kind, ActionNone)
	}
}

func TestCheckInstance_BootstrapStalled(t *testing.T) {
	launchedAt := time.Now().Add(-25 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		JobState: JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:      time.Now(),
	})
	if action.Kind != ActionBootstrapStalled {
		t.Fatalf("action.Kind = %d, want ActionBootstrapStalled (%d)", action.Kind, ActionBootstrapStalled)
	}
	if action.TerminalStatus != db.LaunchStatusFailed {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusFailed)
	}
}

func TestCheckInstance_BootstrapWarnOnly(t *testing.T) {
	launchedAt := time.Now().Add(-16 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		JobState: JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:      time.Now(),
	})
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %d, want ActionDisplayOnly (%d)", action.Kind, ActionDisplayOnly)
	}
	if action.StallMessage == "" {
		t.Error("StallMessage should be non-empty for bootstrap warning")
	}
}

func TestCheckInstance_AdaptiveBootstrapTimeout(t *testing.T) {
	// With a custom 6-minute terminate timeout, an instance at 7 minutes
	// should be terminated (even though it's under the default 20m).
	launchedAt := time.Now().Add(-7 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		JobState: JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:      time.Now(),
		BootstrapSurvival: &db.BootstrapSurvival{
			WarnAfter:      4 * time.Minute,
			TerminateAfter: 6 * time.Minute,
		},
	})
	if action.Kind != ActionBootstrapStalled {
		t.Fatalf("action.Kind = %d, want ActionBootstrapStalled (%d)", action.Kind, ActionBootstrapStalled)
	}
	if !strings.Contains(action.StallMessage, "bootstrap timeout after") {
		t.Errorf("StallMessage = %q, want 'bootstrap timeout after...'", action.StallMessage)
	}
}

func TestCheckInstance_AdaptiveBootstrapWarn(t *testing.T) {
	// With a custom 4-minute warn, 6-minute terminate: at 5 minutes,
	// should warn but not terminate.
	launchedAt := time.Now().Add(-5 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		JobState: JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:      time.Now(),
		BootstrapSurvival: &db.BootstrapSurvival{
			WarnAfter:      4 * time.Minute,
			TerminateAfter: 6 * time.Minute,
		},
	})
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %d, want ActionDisplayOnly (%d)", action.Kind, ActionDisplayOnly)
	}
	if !strings.Contains(action.StallMessage, "terminating in") {
		t.Errorf("StallMessage = %q, should contain remaining time", action.StallMessage)
	}
}

func TestCheckInstance_BootstrapUsesProviderRunningAt(t *testing.T) {
	// LaunchedAt is 25 minutes ago (would trigger timeout), but
	// ProviderRunningAt is only 2 minutes ago (within threshold).
	// Should NOT trigger bootstrap stall.
	launchedAt := time.Now().Add(-25 * time.Minute).Unix()
	providerRunningAt := time.Now().Add(-2 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                1,
			Status:            db.LaunchStatusRunning,
			LaunchedAt:        &launchedAt,
			ProviderRunningAt: &providerRunningAt,
		},
		JobState: JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:      time.Now(),
	})
	if action.Kind == ActionBootstrapStalled {
		t.Fatalf("should not trigger bootstrap stall when ProviderRunningAt is recent")
	}
}

func TestCheckInstance_BootstrapFallsBackToLaunchedAt(t *testing.T) {
	// When ProviderRunningAt is nil, should fall back to LaunchedAt.
	launchedAt := time.Now().Add(-25 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		JobState: JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:      time.Now(),
	})
	if action.Kind != ActionBootstrapStalled {
		t.Fatalf("action.Kind = %d, want ActionBootstrapStalled (%d)", action.Kind, ActionBootstrapStalled)
	}
}

func TestCheckInstance_SelfDestructFailed(t *testing.T) {
	r := NewReconciler()
	latestEnd := time.Now().Add(-3 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:     1,
			Status: db.LaunchStatusRunning,
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		JobState:     JobState{HasStartedJob: true, AllJobsTerminal: true, AllJobsCompleted: true, LatestJobEnd: latestEnd},
		Now:          time.Now(),
	})
	if action.Kind != ActionSelfDestructFailed {
		t.Fatalf("action.Kind = %d, want ActionSelfDestructFailed (%d)", action.Kind, ActionSelfDestructFailed)
	}
	if action.TerminalStatus != db.LaunchStatusCompleted {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusCompleted)
	}
}

func TestCheckInstance_SelfDestructFailed_FailedJobsMarksLaunchFailed(t *testing.T) {
	r := NewReconciler()
	latestEnd := time.Now().Add(-3 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:     1,
			Status: db.LaunchStatusRunning,
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		JobState: JobState{
			HasStartedJob:   true,
			AllJobsTerminal: true,
			LatestJobEnd:    latestEnd,
		},
		Now: time.Now(),
	})
	if action.Kind != ActionSelfDestructFailed {
		t.Fatalf("action.Kind = %d, want ActionSelfDestructFailed (%d)", action.Kind, ActionSelfDestructFailed)
	}
	if action.TerminalStatus != db.LaunchStatusFailed {
		t.Fatalf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusFailed)
	}
	if action.TerminationReason != db.TerminationReasonJobFailure {
		t.Fatalf("TerminationReason = %q, want %q", action.TerminationReason, db.TerminationReasonJobFailure)
	}
}

func TestCheckInstance_SelfDestructFailed_BeatsStaleHeartbeatWarning(t *testing.T) {
	r := NewReconciler()
	latestEnd := time.Now().Add(-3 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:     1,
			Status: db.LaunchStatusRunning,
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		HeartbeatAge: 5 * time.Minute,
		JobState:     JobState{HasStartedJob: true, AllJobsTerminal: true, AllJobsCompleted: true, LatestJobEnd: latestEnd},
		Now:          time.Now(),
	})
	if action.Kind != ActionSelfDestructFailed {
		t.Fatalf("action.Kind = %d, want ActionSelfDestructFailed (%d)", action.Kind, ActionSelfDestructFailed)
	}
}

func TestCheckInstance_SelfDestructFailed_ZeroLatestJobEnd(t *testing.T) {
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:     1,
			Status: db.LaunchStatusRunning,
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		JobState:     JobState{HasStartedJob: true, AllJobsTerminal: true, AllJobsCompleted: true, LatestJobEnd: 0},
		Now:          time.Now(),
	})
	if action.Kind != ActionSelfDestructFailed {
		t.Fatalf("action.Kind = %d, want ActionSelfDestructFailed (%d); LatestJobEnd=0 should still trigger cleanup", action.Kind, ActionSelfDestructFailed)
	}
	if action.TerminalStatus != db.LaunchStatusCompleted {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusCompleted)
	}
}

func TestCheckInstance_EmptyStatusTimeout(t *testing.T) {
	launchedAt := time.Now().Add(-5 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: ""},
		Now:          time.Now(),
	})
	if action.Kind != ActionEmptyStatusTimeout {
		t.Fatalf("action.Kind = %d, want ActionEmptyStatusTimeout (%d)", action.Kind, ActionEmptyStatusTimeout)
	}
}

func TestCheckInstance_TerminationIntent_Completed(t *testing.T) {
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		TerminationIntent: &instanceintent.Marker{
			TerminalStatus:    db.LaunchStatusCompleted,
			TerminationReason: db.TerminationReasonCompleted,
		},
		Now: time.Now(),
	})
	if action.Kind != ActionTerminationIntent {
		t.Fatalf("action.Kind = %d, want ActionTerminationIntent (%d)", action.Kind, ActionTerminationIntent)
	}
	if action.TerminalStatus != db.LaunchStatusCompleted {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusCompleted)
	}
	if !action.DestroyProvider {
		t.Error("DestroyProvider should be true for running provider")
	}
}

func TestCheckInstance_TerminationIntent_Failed(t *testing.T) {
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		TerminationIntent: &instanceintent.Marker{
			TerminalStatus:    db.LaunchStatusFailed,
			TerminationReason: db.TerminationReasonDiskFull,
		},
		Now: time.Now(),
	})
	if action.Kind != ActionTerminationIntent {
		t.Fatalf("action.Kind = %d, want ActionTerminationIntent (%d)", action.Kind, ActionTerminationIntent)
	}
	if action.TerminalStatus != db.LaunchStatusFailed {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusFailed)
	}
	if !action.ResetJobs {
		t.Error("ResetJobs should be true for failed termination")
	}
}

func TestCheckInstance_ProviderDead_WithHysteresis(t *testing.T) {
	r := NewReconciler()
	now := time.Now()
	params := CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusExited},
		Now:          now,
	}

	// First check: should be ActionNone (waiting to confirm)
	action := r.CheckInstance(params)
	if action.Kind != ActionNone {
		t.Fatalf("first check: action.Kind = %d, want ActionNone", action.Kind)
	}

	// Second check after confirm time: should detect dead
	params.Now = now.Add(minDeadConfirmTime + time.Second)
	action = r.CheckInstance(params)
	if action.Kind != ActionProviderDead {
		t.Fatalf("second check: action.Kind = %d, want ActionProviderDead (%d)", action.Kind, ActionProviderDead)
	}
	if action.TerminalStatus != db.LaunchStatusFailed {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusFailed)
	}
}

func TestCheckInstance_ProviderDead_NoHysteresis(t *testing.T) {
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1, // disable hysteresis
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusExited},
		Now:          time.Now(),
	})
	if action.Kind != ActionProviderDead {
		t.Fatalf("action.Kind = %d, want ActionProviderDead (%d)", action.Kind, ActionProviderDead)
	}
}

func TestCheckInstance_ProviderDead_BeatsStaleHeartbeatWarning(t *testing.T) {
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusExited},
		HeartbeatAge: 5 * time.Minute,
		Now:          time.Now(),
	})
	if action.Kind != ActionProviderDead {
		t.Fatalf("action.Kind = %d, want ActionProviderDead (%d)", action.Kind, ActionProviderDead)
	}
}

func TestCheckInstance_ProviderDead_UsesTerminalJobsForCompletedLaunch(t *testing.T) {
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusExited},
		JobState: JobState{
			HasStartedJob:    true,
			AllJobsTerminal:  true,
			AllJobsCompleted: true,
			LatestJobEnd:     time.Now().Add(-2 * time.Minute).Unix(),
		},
		Now: time.Now(),
	})
	if action.TerminalStatus != db.LaunchStatusCompleted {
		t.Fatalf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusCompleted)
	}
}

func TestCheckInstance_ProviderDead_SkipsGraceInstances(t *testing.T) {
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	deadline := time.Now().Add(5 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusGrace,
			ProviderInstanceID: "test-123",
			GraceDeadline:      &deadline,
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusExited},
		Now:          time.Now(),
	})
	if action.Kind != ActionNone {
		t.Fatalf("action.Kind = %d, want ActionNone (grace instances should skip dead detection)", action.Kind)
	}
}

func TestComputeJobState_HistoricalOrphanedAttemptCountsAsStartedAndTerminal(t *testing.T) {
	end := time.Now().Add(-2 * time.Minute).Unix()
	jobs := []*db.Job{{
		ID:      88,
		Status:  db.StatusQueued,
		EndTime: &end,
	}}
	state := ComputeJobState(jobs, map[int64]string{88: db.AttemptOutcomeOrphaned})
	if !state.HasStartedJob {
		t.Fatal("HasStartedJob = false, want true")
	}
	if !state.AllJobsTerminal {
		t.Fatal("AllJobsTerminal = false, want true")
	}
	if state.LatestJobEnd != end {
		t.Fatalf("LatestJobEnd = %d, want %d", state.LatestJobEnd, end)
	}
	if got, reason, ok := state.TerminalLaunchStatus(); !ok || got != db.LaunchStatusFailed || reason != db.TerminationReasonInfraFailure {
		t.Fatalf("TerminalLaunchStatus() = (%q, %q, %v), want (%q, %q, true)", got, reason, ok, db.LaunchStatusFailed, db.TerminationReasonInfraFailure)
	}
}

func TestComputeJobState_MixedFailedAndOrphanedUsesJobFailure(t *testing.T) {
	endA := time.Now().Add(-2 * time.Minute).Unix()
	endB := time.Now().Add(-90 * time.Second).Unix()
	jobs := []*db.Job{
		{ID: 88, Status: db.StatusQueued, EndTime: &endA},
		{ID: 89, Status: db.StatusQueued, EndTime: &endB},
	}
	state := ComputeJobState(jobs, map[int64]string{
		88: db.AttemptOutcomeOrphaned,
		89: db.AttemptOutcomeFailed,
	})
	if got, reason, ok := state.TerminalLaunchStatus(); !ok || got != db.LaunchStatusFailed || reason != db.TerminationReasonJobFailure {
		t.Fatalf("TerminalLaunchStatus() = (%q, %q, %v), want (%q, %q, true)", got, reason, ok, db.LaunchStatusFailed, db.TerminationReasonJobFailure)
	}
}

func TestComputeJobState_DeadAndKilledAreTerminal(t *testing.T) {
	deadEnd := time.Now().Add(-3 * time.Minute).Unix()
	killedEnd := time.Now().Add(-1 * time.Minute).Unix()
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusDead, EndTime: &deadEnd},
		{ID: 2, Status: db.StatusKilled, EndTime: &killedEnd},
	}
	state := ComputeJobState(jobs, nil)
	if !state.AllJobsTerminal {
		t.Fatal("AllJobsTerminal = false, want true")
	}
	if state.LatestJobEnd != killedEnd {
		t.Fatalf("LatestJobEnd = %d, want %d", state.LatestJobEnd, killedEnd)
	}
}

func TestCheckInstance_NilCI(t *testing.T) {
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{Now: time.Now()})
	if action.Kind != ActionNone {
		t.Fatalf("action.Kind = %d, want ActionNone for nil CI", action.Kind)
	}
}

func TestCheckInstance_StaleCreatedStatus(t *testing.T) {
	launchedAt := time.Now().Add(-6 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusCreated},
		Now:          time.Now(),
	})
	if action.Kind != ActionEmptyStatusTimeout {
		t.Fatalf("action.Kind = %d, want ActionEmptyStatusTimeout (%d)", action.Kind, ActionEmptyStatusTimeout)
	}
	if !action.ResetJobs {
		t.Error("expected ResetJobs to be true")
	}
}

func TestCheckInstance_CreatedStatusUnderTimeout(t *testing.T) {
	launchedAt := time.Now().Add(-2 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusCreated},
		Now:          time.Now(),
	})
	if action.Kind != ActionNone {
		t.Fatalf("action.Kind = %d, want ActionNone (%d) — instance still within timeout", action.Kind, ActionNone)
	}
}

func TestCheckInstance_StaleLoadingStatus(t *testing.T) {
	launchedAt := time.Now().Add(-6 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusLoading},
		Now:          time.Now(),
	})
	if action.Kind != ActionEmptyStatusTimeout {
		t.Fatalf("action.Kind = %d, want ActionEmptyStatusTimeout (%d)", action.Kind, ActionEmptyStatusTimeout)
	}
	if !action.ResetJobs {
		t.Error("expected ResetJobs to be true")
	}
}

func TestCheckInstance_LoadingStatusUnderTimeout(t *testing.T) {
	launchedAt := time.Now().Add(-2 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusLoading},
		Now:          time.Now(),
	})
	if action.Kind != ActionNone {
		t.Fatalf("action.Kind = %d, want ActionNone (%d) — instance still within timeout", action.Kind, ActionNone)
	}
}

func TestCheckInstance_LoadingStatusUsesCreatedAtFallback(t *testing.T) {
	createdAt := time.Now().Add(-6 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			CreatedAt:          createdAt,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusLoading},
		Now:          time.Now(),
	})
	if action.Kind != ActionEmptyStatusTimeout {
		t.Fatalf("action.Kind = %d, want ActionEmptyStatusTimeout (%d)", action.Kind, ActionEmptyStatusTimeout)
	}
}

func TestCheckInstance_ProviderStatusUnavailableTimesOut(t *testing.T) {
	launchedAt := time.Now().Add(-7 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderErr: fmt.Errorf("provider API timeout"),
		JobState:    JobState{},
		Now:         time.Now(),
	})
	if action.Kind != ActionEmptyStatusTimeout {
		t.Fatalf("action.Kind = %d, want ActionEmptyStatusTimeout (%d)", action.Kind, ActionEmptyStatusTimeout)
	}
	if !action.ResetJobs {
		t.Error("expected ResetJobs to be true")
	}
}

func TestCheckInstance_PauseTolerant_RecentPause_Pause(t *testing.T) {
	// Running launch whose provider instance just transitioned to stopped:
	// flip the launch to paused.
	r := NewReconciler()
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
			CreatedAt:          launchedAt,
			LaunchedAt:         &launchedAt,
		},
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusStopped},
		PauseTolerant: true,
		Now:           now,
	})
	if action.Kind != ActionPause {
		t.Fatalf("action.Kind = %d, want ActionPause (%d)", action.Kind, ActionPause)
	}
}

func TestCheckInstance_AlreadyPaused_DisplayOnly(t *testing.T) {
	// Launch already in paused state, provider still stopped: keep paused, no
	// repeated DB writes.
	r := NewReconciler()
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusPaused,
			ProviderInstanceID: "test-123",
			CreatedAt:          launchedAt,
			LaunchedAt:         &launchedAt,
		},
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusStopped},
		PauseTolerant: true,
		Now:           now,
	})
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %d, want ActionDisplayOnly (%d)", action.Kind, ActionDisplayOnly)
	}
}

func TestCheckInstance_PausedLaunchResumed(t *testing.T) {
	// Launch is paused, provider now reports running again: emit ActionResume.
	r := NewReconciler()
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusPaused,
			ProviderInstanceID: "test-123",
			CreatedAt:          launchedAt,
			LaunchedAt:         &launchedAt,
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		Now:          now,
	})
	if action.Kind != ActionResume {
		t.Fatalf("action.Kind = %d, want ActionResume (%d)", action.Kind, ActionResume)
	}
}

func TestCheckInstance_PauseTolerant_StalePause_Preempted(t *testing.T) {
	// Interruptible instance paused past stalePauseTimeout: should be terminated
	// with TerminationReasonPreempted and jobs reset for relaunch.
	r := NewReconciler()
	now := time.Now()
	launchedAt := now.Add(-7 * time.Hour).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
			CreatedAt:          launchedAt,
			LaunchedAt:         &launchedAt,
		},
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusStopped},
		PauseTolerant: true,
		Now:           now,
	})
	if action.Kind != ActionEmptyStatusTimeout {
		t.Fatalf("action.Kind = %d, want ActionEmptyStatusTimeout (%d)", action.Kind, ActionEmptyStatusTimeout)
	}
	if action.TerminationReason != db.TerminationReasonPreempted {
		t.Errorf("TerminationReason = %q, want %q", action.TerminationReason, db.TerminationReasonPreempted)
	}
	if !action.ResetJobs {
		t.Error("expected ResetJobs=true so relaunch picks a fresh offer")
	}
	if !action.DestroyProvider {
		t.Error("expected DestroyProvider=true")
	}
}

func TestCheckInstance_IntendedStatusStopped(t *testing.T) {
	// Provider allocated but intended_status is "stopped" while actual_status is "created".
	// isProviderTerminal should detect this immediately via intended_status check.
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1, // disable hysteresis
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusCreated, IntendedStatus: cloud.ProviderStatusStopped},
		Now:          time.Now(),
	})
	if action.Kind != ActionProviderDead {
		t.Fatalf("action.Kind = %d, want ActionProviderDead (%d)", action.Kind, ActionProviderDead)
	}
	if !action.ResetJobs {
		t.Error("expected ResetJobs to be true for dead instance")
	}
}

func TestCheckInstance_IntendedStatusStoppedButRunning(t *testing.T) {
	// If Status is "running", IntendedStatus "stopped" should NOT trigger
	// provider-terminal — the instance is still alive and the stop hasn't taken effect.
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning, IntendedStatus: cloud.ProviderStatusStopped},
		JobState:     JobState{HasStartedJob: true},
		Now:          time.Now(),
	})
	if action.Kind != ActionNone {
		t.Fatalf("action.Kind = %d, want ActionNone (%d) — running overrides intended stop", action.Kind, ActionNone)
	}
}

func TestCheckInstance_HeartbeatStale_DisplayOnly(t *testing.T) {
	r := NewReconciler()
	// LatestJobEnd is recent (< 2 min ago), so step 6 (self-destruct) defers;
	// instead the stale heartbeat warning fires.
	recentEnd := time.Now().Add(-30 * time.Second).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		HeartbeatAge: 5 * time.Minute,
		JobState:     JobState{HasStartedJob: true, AllJobsTerminal: true, LatestJobEnd: recentEnd},
		Now:          time.Now(),
	})
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %d, want ActionDisplayOnly (%d)", action.Kind, ActionDisplayOnly)
	}
	if action.StallMessage == "" {
		t.Error("StallMessage should be non-empty for stale heartbeat")
	}
}

func TestCheckInstance_SetupStall_Terminate(t *testing.T) {
	launchedAt := time.Now().Add(-30 * time.Minute).Unix()
	phaseStart := time.Now().Add(-26 * time.Minute)
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:  "setup:459",
		PhaseChangedAt: &phaseStart,
		JobState:       JobState{HasStartedJob: true},
		Now:            time.Now(),
	})
	if action.Kind != ActionSetupStalled {
		t.Fatalf("action.Kind = %d, want ActionSetupStalled (%d)", action.Kind, ActionSetupStalled)
	}
	if action.TerminalStatus != db.LaunchStatusFailed {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusFailed)
	}
	if !action.ResetJobs {
		t.Error("ResetJobs should be true")
	}
}

func TestCheckInstance_SetupStall_Warn(t *testing.T) {
	launchedAt := time.Now().Add(-20 * time.Minute).Unix()
	phaseStart := time.Now().Add(-16 * time.Minute)
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:  "setup:459",
		PhaseChangedAt: &phaseStart,
		JobState:       JobState{HasStartedJob: true},
		Now:            time.Now(),
	})
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %d, want ActionDisplayOnly (%d)", action.Kind, ActionDisplayOnly)
	}
	if action.StallMessage == "" {
		t.Error("StallMessage should be non-empty")
	}
}

func TestCheckInstance_SetupStall_UnderThreshold(t *testing.T) {
	launchedAt := time.Now().Add(-12 * time.Minute).Unix()
	phaseStart := time.Now().Add(-10 * time.Minute)
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:  "setup:459",
		PhaseChangedAt: &phaseStart,
		JobState:       JobState{HasStartedJob: true},
		Now:            time.Now(),
	})
	if action.Kind != ActionNone {
		t.Fatalf("action.Kind = %d, want ActionNone (%d)", action.Kind, ActionNone)
	}
}

func TestCheckInstance_SetupStall_NonSetupPhase(t *testing.T) {
	launchedAt := time.Now().Add(-30 * time.Minute).Unix()
	phaseStart := time.Now().Add(-26 * time.Minute)
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:  "running:459",
		PhaseChangedAt: &phaseStart,
		JobState:       JobState{HasStartedJob: true},
		Now:            time.Now(),
	})
	// Running/uploading phases should NOT trigger setup stall
	if action.Kind == ActionSetupStalled {
		t.Fatalf("action.Kind = ActionSetupStalled, but non-setup phase should not trigger stall")
	}
}

func TestCheckInstance_SetupStall_NilPhaseChangedAt_UsesLifecycleFallback(t *testing.T) {
	launchedAt := time.Now().Add(-30 * time.Minute).Unix()
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase: "setup:459",
		// PhaseChangedAt is nil — should fall back to instance lifecycle start.
		JobState: JobState{HasStartedJob: true},
		Now:      time.Now(),
	})
	if action.Kind != ActionSetupStalled {
		t.Fatalf("action.Kind = %d, want ActionSetupStalled (%d)", action.Kind, ActionSetupStalled)
	}
}

func TestCheckInstance_SetupStall_StaleR2SetupButDBRunning_DoesNotTerminate(t *testing.T) {
	launchedAt := time.Now().Add(-3 * time.Hour).Unix()
	phaseStart := time.Now().Add(-3 * time.Hour)
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:  "running:1189", // reconciled phase from DB authority
		PhaseChangedAt: &phaseStart,
		JobState:       JobState{HasStartedJob: true},
		Now:            time.Now(),
	})
	if action.Kind == ActionSetupStalled {
		t.Fatalf("action.Kind = ActionSetupStalled, want non-setup behavior")
	}
}

func TestCheckInstance_SetupStall_QueuedJobNoPhaseChangedAt_Terminates(t *testing.T) {
	launchedAt := time.Now().Add(-30 * time.Minute).Unix()
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase: "setup:1189",
		// PhaseChangedAt intentionally missing; should use lifecycle fallback.
		JobState: JobState{HasStartedJob: false},
		Now:      time.Now(),
	})
	if action.Kind != ActionSetupStalled {
		t.Fatalf("action.Kind = %d, want ActionSetupStalled (%d)", action.Kind, ActionSetupStalled)
	}
}

func TestCheckInstance_SetupStall_CustomSurvival(t *testing.T) {
	launchedAt := time.Now().Add(-12 * time.Minute).Unix()
	phaseStart := time.Now().Add(-8 * time.Minute)
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	// Custom survival with very short thresholds
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:  "setup:459",
		PhaseChangedAt: &phaseStart,
		SetupSurvival: &db.SetupSurvival{
			SampleSize:     50,
			WarnAfter:      5 * time.Minute,
			TerminateAfter: 7 * time.Minute,
		},
		JobState: JobState{HasStartedJob: true},
		Now:      time.Now(),
	})
	if action.Kind != ActionSetupStalled {
		t.Fatalf("action.Kind = %d, want ActionSetupStalled (%d) with custom survival thresholds", action.Kind, ActionSetupStalled)
	}
}

func TestCheckInstance_RunningStall_Terminates(t *testing.T) {
	launchedAt := time.Now().Add(-60 * time.Minute).Unix()
	phaseStart := time.Now().Add(-35 * time.Minute)
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:  "running:531",
		PhaseChangedAt: &phaseStart,
		HeartbeatAge:   10 * time.Minute, // stale
		JobState:       JobState{HasStartedJob: true},
		Now:            time.Now(),
	})
	if action.Kind != ActionRunningStalled {
		t.Fatalf("action.Kind = %d, want ActionRunningStalled (%d)", action.Kind, ActionRunningStalled)
	}
	if action.TerminalStatus != db.LaunchStatusFailed {
		t.Fatalf("terminal status = %q, want %q", action.TerminalStatus, db.LaunchStatusFailed)
	}
	if !action.DestroyProvider {
		t.Fatal("expected DestroyProvider = true")
	}
	if !action.ResetJobs {
		t.Fatal("expected ResetJobs = true")
	}
}

func TestCheckInstance_RunningStall_WarnsBeforeTermination(t *testing.T) {
	launchedAt := time.Now().Add(-30 * time.Minute).Unix()
	phaseStart := time.Now().Add(-15 * time.Minute) // past warn threshold, before terminate
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:  "running:531",
		PhaseChangedAt: &phaseStart,
		HeartbeatAge:   10 * time.Minute, // stale
		JobState:       JobState{HasStartedJob: true},
		Now:            time.Now(),
	})
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %d, want ActionDisplayOnly (%d) for warn phase", action.Kind, ActionDisplayOnly)
	}
	if action.StallMessage == "" {
		t.Fatal("expected non-empty stall message")
	}
}

func TestCheckInstance_RunningStall_FreshHeartbeatNoAction(t *testing.T) {
	launchedAt := time.Now().Add(-60 * time.Minute).Unix()
	phaseStart := time.Now().Add(-35 * time.Minute) // past terminate threshold
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:  "running:531",
		PhaseChangedAt: &phaseStart,
		HeartbeatAge:   30 * time.Second, // fresh — agent is alive
		JobState:       JobState{HasStartedJob: true},
		Now:            time.Now(),
	})
	// Fresh heartbeat means the agent is alive — do NOT terminate even with old phase
	if action.Kind == ActionRunningStalled {
		t.Fatal("should not trigger running stall when heartbeat is fresh")
	}
}

func TestCheckInstance_RunningStall_ZeroHeartbeatAgeNoAction(t *testing.T) {
	launchedAt := time.Now().Add(-60 * time.Minute).Unix()
	phaseStart := time.Now().Add(-35 * time.Minute)
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:         1,
			Status:     db.LaunchStatusRunning,
			LaunchedAt: &launchedAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:  "running:531",
		PhaseChangedAt: &phaseStart,
		HeartbeatAge:   0, // no heartbeat data yet
		JobState:       JobState{HasStartedJob: true},
		Now:            time.Now(),
	})
	if action.Kind == ActionRunningStalled {
		t.Fatal("should not trigger running stall when heartbeat age is 0 (unknown)")
	}
}
