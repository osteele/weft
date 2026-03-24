package campaign

import (
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
		CI: &db.CloudInstance{
			ID:            1,
			Status:        db.CloudInstanceStatusGrace,
			GraceDeadline: &pastDeadline,
		},
		Now: time.Now(),
	})
	if action.Kind != ActionGraceExpired {
		t.Fatalf("action.Kind = %d, want ActionGraceExpired (%d)", action.Kind, ActionGraceExpired)
	}
	if action.TerminalStatus != db.CloudInstanceStatusFailed {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.CloudInstanceStatusFailed)
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
		CI: &db.CloudInstance{
			ID:            1,
			Status:        db.CloudInstanceStatusGrace,
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
		CI: &db.CloudInstance{
			ID:         1,
			Status:     db.CloudInstanceStatusRunning,
			LaunchedAt: &launchedAt,
		},
		JobState: JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:      time.Now(),
	})
	if action.Kind != ActionBootstrapStalled {
		t.Fatalf("action.Kind = %d, want ActionBootstrapStalled (%d)", action.Kind, ActionBootstrapStalled)
	}
	if action.TerminalStatus != db.CloudInstanceStatusFailed {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.CloudInstanceStatusFailed)
	}
}

func TestCheckInstance_BootstrapWarnOnly(t *testing.T) {
	launchedAt := time.Now().Add(-16 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.CloudInstance{
			ID:         1,
			Status:     db.CloudInstanceStatusRunning,
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

func TestCheckInstance_SelfDestructFailed(t *testing.T) {
	r := NewReconciler()
	latestEnd := time.Now().Add(-3 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.CloudInstance{
			ID:     1,
			Status: db.CloudInstanceStatusRunning,
		},
		ProviderInst: &cloud.Instance{Status: "running"},
		JobState:     JobState{HasStartedJob: true, AllJobsTerminal: true, LatestJobEnd: latestEnd},
		Now:          time.Now(),
	})
	if action.Kind != ActionSelfDestructFailed {
		t.Fatalf("action.Kind = %d, want ActionSelfDestructFailed (%d)", action.Kind, ActionSelfDestructFailed)
	}
	if action.TerminalStatus != db.CloudInstanceStatusCompleted {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.CloudInstanceStatusCompleted)
	}
}

func TestCheckInstance_EmptyStatusTimeout(t *testing.T) {
	launchedAt := time.Now().Add(-5 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
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
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: "running"},
		TerminationIntent: &instanceintent.Marker{
			TerminalStatus:    db.CloudInstanceStatusCompleted,
			TerminationReason: db.TerminationReasonCompleted,
		},
		Now: time.Now(),
	})
	if action.Kind != ActionTerminationIntent {
		t.Fatalf("action.Kind = %d, want ActionTerminationIntent (%d)", action.Kind, ActionTerminationIntent)
	}
	if action.TerminalStatus != db.CloudInstanceStatusCompleted {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.CloudInstanceStatusCompleted)
	}
	if !action.DestroyProvider {
		t.Error("DestroyProvider should be true for running provider")
	}
}

func TestCheckInstance_TerminationIntent_Failed(t *testing.T) {
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: "running"},
		TerminationIntent: &instanceintent.Marker{
			TerminalStatus:    db.CloudInstanceStatusFailed,
			TerminationReason: db.TerminationReasonDiskFull,
		},
		Now: time.Now(),
	})
	if action.Kind != ActionTerminationIntent {
		t.Fatalf("action.Kind = %d, want ActionTerminationIntent (%d)", action.Kind, ActionTerminationIntent)
	}
	if action.TerminalStatus != db.CloudInstanceStatusFailed {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.CloudInstanceStatusFailed)
	}
	if !action.ResetJobs {
		t.Error("ResetJobs should be true for failed termination")
	}
}

func TestCheckInstance_ProviderDead_WithHysteresis(t *testing.T) {
	r := NewReconciler()
	now := time.Now()
	params := CheckInstanceParams{
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: "exited"},
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
	if action.TerminalStatus != db.CloudInstanceStatusFailed {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.CloudInstanceStatusFailed)
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
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: "exited"},
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
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusGrace,
			ProviderInstanceID: "test-123",
			GraceDeadline:      &deadline,
		},
		ProviderInst: &cloud.Instance{Status: "exited"},
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
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: "created"},
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
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: "created"},
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
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: "loading"},
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
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: "loading"},
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
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: "created", IntendedStatus: "stopped"},
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
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: "running", IntendedStatus: "stopped"},
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
		CI: &db.CloudInstance{
			ID:                 1,
			Status:             db.CloudInstanceStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: "running"},
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
