package campaign

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
)

func TestCheckInstance_GraceExpired(t *testing.T) {
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
	if !action.ResetJobs {
		t.Error("ResetJobs should be true")
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

func TestCheckInstance_SelfDestructFailed(t *testing.T) {
	r := NewReconciler()
	latestEnd := time.Now().Add(-3 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:     1,
			Status: db.LaunchStatusRunning,
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		JobState:     JobState{HasStartedJob: true, AllJobsTerminal: true, LatestJobEnd: latestEnd},
		Now:          time.Now(),
	})
	if action.Kind != ActionSelfDestructFailed {
		t.Fatalf("action.Kind = %d, want ActionSelfDestructFailed (%d)", action.Kind, ActionSelfDestructFailed)
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
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		HeartbeatAge: 5 * time.Minute,
		JobState:     JobState{HasStartedJob: true, AllJobsTerminal: true},
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

func TestCheckInstance_SetupStall_NilPhaseChangedAt(t *testing.T) {
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
		// PhaseChangedAt is nil — should be skipped
		JobState: JobState{HasStartedJob: true},
		Now:      time.Now(),
	})
	if action.Kind == ActionSetupStalled {
		t.Fatalf("should not trigger setup stall when PhaseChangedAt is nil")
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
