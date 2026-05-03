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
	expiredDeadline := time.Now().Add(-5 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                    1,
			Status:                db.LaunchStatusRunning,
			LaunchedAt:            &launchedAt,
			BootstrapDeadlineUnix: &expiredDeadline,
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
	// Deadline 4 minutes in the future = inside the warn window
	// (BootstrapTerminateTimeout - bootstrapWarnTimeout = 10m).
	launchedAt := time.Now().Add(-16 * time.Minute).Unix()
	warnDeadline := time.Now().Add(4 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                    1,
			Status:                db.LaunchStatusRunning,
			LaunchedAt:            &launchedAt,
			BootstrapDeadlineUnix: &warnDeadline,
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

func TestCheckInstance_BootstrapSurvivalExtendsExistingDeadline(t *testing.T) {
	now := time.Unix(100000, 0)
	launchedAt := now.Add(-25 * time.Minute).Unix()
	expiredDeadline := now.Add(-5 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                    1,
			Status:                db.LaunchStatusRunning,
			LaunchedAt:            &launchedAt,
			BootstrapDeadlineUnix: &expiredDeadline,
		},
		BootstrapSurvival: &db.BootstrapSurvival{
			SampleSize:     50,
			WarnAfter:      20 * time.Minute,
			TerminateAfter: 30 * time.Minute,
		},
		JobState: JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:      now,
	})
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %d, want ActionDisplayOnly (%d)", action.Kind, ActionDisplayOnly)
	}
}

func TestCheckInstance_BootstrapSurvivalTerminatesAfterLearnedDeadline(t *testing.T) {
	now := time.Unix(100000, 0)
	launchedAt := now.Add(-31 * time.Minute).Unix()
	expiredDeadline := now.Add(-11 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                    1,
			Status:                db.LaunchStatusRunning,
			LaunchedAt:            &launchedAt,
			BootstrapDeadlineUnix: &expiredDeadline,
		},
		BootstrapSurvival: &db.BootstrapSurvival{
			SampleSize:     50,
			WarnAfter:      20 * time.Minute,
			TerminateAfter: 30 * time.Minute,
		},
		JobState: JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:      now,
	})
	if action.Kind != ActionBootstrapStalled {
		t.Fatalf("action.Kind = %d, want ActionBootstrapStalled (%d)", action.Kind, ActionBootstrapStalled)
	}
}

func TestCheckInstance_BootstrapNoStallWithoutDeadline(t *testing.T) {
	// Defensive: a launch without a deadline (legacy row that somehow
	// escaped the backfill migration) is not stalled by the reconciler;
	// other paths catch it (e.g. launching-phase catch-all).
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
	if action.Kind == ActionBootstrapStalled {
		t.Fatalf("action.Kind = ActionBootstrapStalled, expected no stall when deadline column is unset")
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
	launchedAt := time.Now().Add(-26 * time.Minute).Unix()
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

func TestCheckInstance_ProviderStatusUnavailableWaitsBeforeTimeout(t *testing.T) {
	launchedAt := time.Now().Add(-7 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderErr: fmt.Errorf("provider instance test-123 missing from batch list"),
		JobState:    JobState{},
		Now:         time.Now(),
	})
	if action.Kind != ActionNone {
		t.Fatalf("action.Kind = %d, want ActionNone (%d)", action.Kind, ActionNone)
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
	launchedAt := time.Now().Add(-90 * time.Minute).Unix()
	phaseStart := time.Now().Add(-65 * time.Minute)
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
	launchedAt := time.Now().Add(-45 * time.Minute).Unix()
	phaseStart := time.Now().Add(-25 * time.Minute) // past warn threshold, before terminate
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

func TestCheckInstance_RunningStall_MissingPhaseChangedAtUsesHeartbeatAge(t *testing.T) {
	launchedAt := time.Now().Add(-60 * time.Minute).Unix()
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
		ProviderErr:   fmt.Errorf("provider instance missing from batch list"),
		InstancePhase: "running:531",
		HeartbeatAge:  65 * time.Minute,
		JobState:      JobState{HasStartedJob: true},
		Now:           time.Now(),
	})
	if action.Kind != ActionRunningStalled {
		t.Fatalf("action.Kind = %d, want ActionRunningStalled (%d)", action.Kind, ActionRunningStalled)
	}
	if !action.ResetJobs {
		t.Fatal("expected ResetJobs = true")
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

func TestCheckInstance_PauseTolerant_OfflineFlipsToPaused(t *testing.T) {
	// Vast.ai reports an interruptible instance as "offline" when it has been
	// preempted (data preserved). Treat the same as "stopped": flip the launch
	// to paused and wait for resume.
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
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusOffline},
		PauseTolerant: true,
		Now:           now,
	})
	if action.Kind != ActionPause {
		t.Fatalf("action.Kind = %d, want ActionPause (%d)", action.Kind, ActionPause)
	}
	if action.ObservedProviderStatus != cloud.ProviderStatusOffline {
		t.Errorf("ObservedProviderStatus = %q, want %q", action.ObservedProviderStatus, cloud.ProviderStatusOffline)
	}
}

func TestCheckInstance_OnDemand_StaleOffline_ProviderFailure(t *testing.T) {
	// On-demand instances also flip to paused when offline (matches the
	// existing stopped-status handling — rule 4a-pause runs regardless of
	// pause-tolerance), but past stalePauseTimeout the termination reason
	// is ProviderFailure rather than Preempted.
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
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusOffline},
		PauseTolerant: false,
		Now:           now,
	})
	if action.Kind != ActionEmptyStatusTimeout {
		t.Fatalf("action.Kind = %d, want ActionEmptyStatusTimeout (%d)", action.Kind, ActionEmptyStatusTimeout)
	}
	if action.TerminationReason != db.TerminationReasonProviderFailure {
		t.Errorf("TerminationReason = %q, want %q", action.TerminationReason, db.TerminationReasonProviderFailure)
	}
}

func TestCheckInstance_PreviouslyRunning_FreshDeadlineForNonRunning(t *testing.T) {
	// Instance launched 30m ago, ran for most of that time, just transitioned
	// to a non-running, non-terminal status (e.g. "loading") two minutes ago.
	// Should not fire rule 4b yet — the 5-minute deadline anchors on the
	// last-status-change, not on launched_at.
	r := NewReconciler()
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	lastChange := now.Add(-2 * time.Minute)
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
			CreatedAt:          launchedAt,
			LaunchedAt:         &launchedAt,
		},
		ProviderInst:               &cloud.Instance{Status: cloud.ProviderStatusLoading},
		LastProviderStatusChangeAt: &lastChange,
		Now:                        now,
	})
	if action.Kind == ActionEmptyStatusTimeout {
		t.Fatalf("rule 4b fired prematurely: action=%+v", action)
	}
}

func TestCheckInstance_PreviouslyRunning_TerminatesAfterFreshDeadline(t *testing.T) {
	// Same setup, but the status has been non-running for 6 minutes — past
	// the 5-minute threshold. Should terminate as infra_failure.
	r := NewReconciler()
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	lastChange := now.Add(-6 * time.Minute)
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
			CreatedAt:          launchedAt,
			LaunchedAt:         &launchedAt,
		},
		ProviderInst:               &cloud.Instance{Status: cloud.ProviderStatusLoading},
		LastProviderStatusChangeAt: &lastChange,
		Now:                        now,
	})
	if action.Kind != ActionEmptyStatusTimeout {
		t.Fatalf("action.Kind = %d, want ActionEmptyStatusTimeout (%d)", action.Kind, ActionEmptyStatusTimeout)
	}
	if action.TerminationReason != db.TerminationReasonInfraFailure {
		t.Errorf("TerminationReason = %q, want %q", action.TerminationReason, db.TerminationReasonInfraFailure)
	}
}

func TestCheckInstance_PauseTolerant_StaleOffline_Preempted(t *testing.T) {
	// Interruptible instance offline past stalePauseTimeout: terminate with
	// TerminationReasonPreempted, parallels the existing stopped variant.
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
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusOffline},
		PauseTolerant: true,
		Now:           now,
	})
	if action.Kind != ActionEmptyStatusTimeout {
		t.Fatalf("action.Kind = %d, want ActionEmptyStatusTimeout (%d)", action.Kind, ActionEmptyStatusTimeout)
	}
	if action.TerminationReason != db.TerminationReasonPreempted {
		t.Errorf("TerminationReason = %q, want %q", action.TerminationReason, db.TerminationReasonPreempted)
	}
}

func TestFormatActionDetail_IncludesProviderSnapshot(t *testing.T) {
	got := formatActionDetail(42, InstanceAction{
		Kind:                           ActionEmptyStatusTimeout,
		TerminationReason:              db.TerminationReasonInfraFailure,
		StallMessage:                   "stuck in offline",
		ObservedProviderStatus:         "offline",
		ObservedProviderIntendedStatus: "running",
		ObservedProviderStatusMsg:      "host unreachable",
	})
	for _, want := range []string{
		"launch_id=42",
		"reason=" + db.TerminationReasonInfraFailure,
		`status="offline"`,
		`intended="running"`,
		`status_msg="host unreachable"`,
		`detail="stuck in offline"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatActionDetail missing %q\nfull: %s", want, got)
		}
	}
}

func TestFormatActionDetail_OmitsEmptyFields(t *testing.T) {
	got := formatActionDetail(7, InstanceAction{Kind: ActionPause})
	if strings.Contains(got, "status=") || strings.Contains(got, "intended=") || strings.Contains(got, "status_msg=") {
		t.Errorf("formatActionDetail leaked empty provider fields: %s", got)
	}
	if !strings.Contains(got, "launch_id=7") {
		t.Errorf("formatActionDetail missing launch_id: %s", got)
	}
}

func TestBuildProviderDeadDetail_PopulatesEvidence(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	launched := now.Add(-3 * time.Minute).Unix()
	ready := now.Add(-90 * time.Second).Unix()
	ci := &db.Launch{
		ID:               42,
		LaunchedAt:       &launched,
		AgentReadyAtUnix: &ready,
	}
	inst := &cloud.Instance{Status: "exited"}
	got := buildProviderDeadDetail(ci, inst, db.TerminationReasonUnknown, db.TerminationReasonUnknown, now)
	for _, want := range []string{
		"provider dead with no completion or intent marker",
		"provider status=exited",
		"launched 3m0s ago",
		"agent ready 1m30s ago",
		"no R2 disk-failure marker",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("buildProviderDeadDetail missing %q\nfull: %s", want, got)
		}
	}
}

func TestBuildProviderDeadDetail_AgentNeverReady(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	launched := now.Add(-45 * time.Second).Unix()
	ci := &db.Launch{ID: 7, LaunchedAt: &launched}
	got := buildProviderDeadDetail(ci, nil, db.TerminationReasonUnknown, db.TerminationReasonUnknown, now)
	for _, want := range []string{
		"provider returned no instance",
		"agent never reported ready",
		"launched 45s ago",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("buildProviderDeadDetail missing %q\nfull: %s", want, got)
		}
	}
}

func TestBuildProviderDeadDetail_R2Refined(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	got := buildProviderDeadDetail(nil, nil, db.TerminationReasonUnknown, db.TerminationReasonDiskFull, now)
	if !strings.Contains(got, "R2 marker refined reason unknown→disk_full") {
		t.Errorf("expected R2 refinement note, got: %s", got)
	}
}
