package campaign

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
)

// A reconciler terminal action is one local state transition. If attempt/job
// cleanup fails, the launch status and execution-target projection must roll
// back with it.
func TestExecuteAction_LocalFailureRollsBackTerminalTransition(t *testing.T) {
	database := setupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "provider-atomicity"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "echo test", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := database.Exec(fmt.Sprintf(`
		CREATE TRIGGER fail_reconcile_job_reset
		BEFORE UPDATE ON jobs
		WHEN OLD.id = %d
		BEGIN
			SELECT RAISE(ABORT, 'injected reconcile reset failure');
		END`, jobID)); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	var destroyCalls atomic.Int32
	client := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		DestroyInstanceFunc: func(string) error {
			if destroyCalls.Add(1) == 1 {
				return nil
			}
			return cloud.ErrInstanceNotFound
		},
	}
	action := InstanceAction{
		Kind:              ActionProviderDead,
		TerminalStatus:    db.LaunchStatusFailed,
		TerminationReason: db.TerminationReasonProviderFailure,
		DestroyProvider:   true,
		ResetJobs:         true,
		AttemptOutcome:    db.AttemptOutcomeOrphaned,
	}
	reconciled, terminated := ExecuteAction(database, client, launch, action)
	if reconciled || terminated {
		t.Fatalf("ExecuteAction = (%v, %v), want rollback result (false, false)", reconciled, terminated)
	}

	launch, err = db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch after ExecuteAction: %v", err)
	}
	if launch.Status != db.LaunchStatusRunning {
		t.Errorf("launch status = %q, want rollback to %q", launch.Status, db.LaunchStatusRunning)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != instanceID {
		t.Errorf("job launch = %v, want rollback to %d", job.LaunchID, instanceID)
	}

	if _, err := database.Exec(`DROP TRIGGER fail_reconcile_job_reset`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	reconciled, terminated = ExecuteAction(database, client, launch, action)
	if !reconciled || !terminated {
		t.Fatalf("ExecuteAction retry = (%v, %v), want confirmed absence to finalize", reconciled, terminated)
	}
}

func TestExecuteAction_DestroyRequiredWithoutProviderClientFailsClosed(t *testing.T) {
	database := setupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "runpod",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "pod-unobservable"); err != nil {
		t.Fatalf("SetLaunchProviderID: %v", err)
	}
	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	reconciled, terminated := ExecuteAction(database, nil, launch, InstanceAction{
		Kind:              ActionRunningStalled,
		TerminalStatus:    db.LaunchStatusFailed,
		TerminationReason: db.TerminationReasonInfraFailure,
		DestroyProvider:   true,
		ResetJobs:         true,
		AttemptOutcome:    db.AttemptOutcomeOrphaned,
	})
	if reconciled || terminated {
		t.Fatalf("ExecuteAction = (%v, %v), want no terminal transition without a provider client", reconciled, terminated)
	}
	launch, err = db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch after ExecuteAction: %v", err)
	}
	if launch.Status != db.LaunchStatusRunning {
		t.Fatalf("launch status = %q, want %q", launch.Status, db.LaunchStatusRunning)
	}
}

func TestExecuteAction_DestroyRequiredWithoutProviderIDFailsClosed(t *testing.T) {
	database := setupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	reconciled, terminated := ExecuteAction(database, &cloud.MockClient{ProviderVal: cloud.ProviderVastai}, launch, InstanceAction{
		Kind:              ActionRunningStalled,
		TerminalStatus:    db.LaunchStatusFailed,
		TerminationReason: db.TerminationReasonInfraFailure,
		DestroyProvider:   true,
		ResetJobs:         true,
		AttemptOutcome:    db.AttemptOutcomeOrphaned,
	})
	if reconciled || terminated {
		t.Fatalf("ExecuteAction = (%v, %v), want no terminal transition without a provider instance ID", reconciled, terminated)
	}
	launch, err = db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch after ExecuteAction: %v", err)
	}
	if launch.Status != db.LaunchStatusRunning {
		t.Fatalf("launch status = %q, want %q", launch.Status, db.LaunchStatusRunning)
	}
}

func TestCheckInstance_GraceExpired_ForceDestroyAfterTimeout(t *testing.T) {
	// Grace expired well past the shutdown timeout — no termination intent from agent.
	// Weft should force-destroy.
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
	if action.TerminationReason != db.TerminationReasonJobFailure {
		t.Errorf("TerminationReason = %q, want %q", action.TerminationReason, db.TerminationReasonJobFailure)
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
	// Weft should wait, not destroy.
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

func TestCheckInstance_TerminalLiveRunningPhase_Terminates(t *testing.T) {
	now := time.Now()
	terminalSince := now.Add(-5 * time.Minute)
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:     1,
			Status: db.LaunchStatusRunning,
		},
		InstancePhase:                "running:537",
		RunningPhaseJobID:            537,
		RunningPhaseJobStatus:        db.StatusFailed,
		RunningPhaseJobTerminalSince: &terminalSince,
		Now:                          now,
	})
	if action.Kind != ActionTerminalLivePhase {
		t.Fatalf("action.Kind = %d, want ActionTerminalLivePhase (%d)", action.Kind, ActionTerminalLivePhase)
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
	if action.AttemptOutcome != db.AttemptOutcomeOrphaned {
		t.Errorf("AttemptOutcome = %q, want %q", action.AttemptOutcome, db.AttemptOutcomeOrphaned)
	}
}

func TestCheckInstance_LiveRunningPhaseStillActive_DoesNotTerminate(t *testing.T) {
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:     1,
			Status: db.LaunchStatusRunning,
		},
		InstancePhase: "running:707",
		Now:           time.Now(),
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
		// OnStart probe landed and bootstrap stage progressed, but
		// then stalled — exactly the scenario rule 5 owns. (The
		// dud-Vast watchdog short-circuits when there is no probe
		// AND no other agent signal.)
		OnStartProbePresent: true,
		JobState:            JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:                 time.Now(),
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
		OnStartProbePresent: true,
		JobState:            JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:                 time.Now(),
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
		OnStartProbePresent: true,
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
		OnStartProbePresent: true,
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

func TestCheckInstance_IdleAfterReadyTimeout_Terminates(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-1 * time.Hour).Unix()
	readyAt := now.Add(-20 * time.Minute).Unix() // past idleAfterReadyTimeout
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:               1,
			Status:           db.LaunchStatusRunning,
			LaunchedAt:       &launchedAt,
			AgentReadyAtUnix: &readyAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		BootstrapStage: bootstrapStageReady,
		JobState:       JobState{HasStartedJob: false},
		InstancePhase:  "",
		HeartbeatAge:   30 * time.Second, // fresh sidecar heartbeat
		Now:            now,
	})
	if action.Kind != ActionIdleAfterReadyTimeout {
		t.Fatalf("action.Kind = %s, want ActionIdleAfterReadyTimeout", action.Kind)
	}
	if action.TerminalStatus != db.LaunchStatusFailed {
		t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusFailed)
	}
	if action.TerminationReason != db.TerminationReasonInfraFailure {
		t.Errorf("TerminationReason = %q, want %q", action.TerminationReason, db.TerminationReasonInfraFailure)
	}
	if !action.DestroyProvider {
		t.Errorf("DestroyProvider = false, want true")
	}
	if !action.ResetJobs {
		t.Errorf("ResetJobs = false, want true")
	}
}

func TestCheckInstance_IdleAfterReadyTimeout_NotYetExpired(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-15 * time.Minute).Unix()
	readyAt := now.Add(-5 * time.Minute).Unix() // before idleAfterReadyTimeout
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:               1,
			Status:           db.LaunchStatusRunning,
			LaunchedAt:       &launchedAt,
			AgentReadyAtUnix: &readyAt,
		},
		ProviderInst:   &cloud.Instance{Status: cloud.ProviderStatusRunning},
		BootstrapStage: bootstrapStageReady,
		JobState:       JobState{HasStartedJob: false},
		Now:            now,
	})
	if action.Kind == ActionIdleAfterReadyTimeout {
		t.Fatalf("action.Kind = ActionIdleAfterReadyTimeout, expected no termination before timeout")
	}
}

func TestCheckInstance_IdleAfterReadyTimeout_SkippedWhenJobStarted(t *testing.T) {
	// agent_ready_at is also stamped when a job starts; once a job has
	// actually started, the running-phase watchdogs cover the instance and
	// this check must not fire even past the idle timeout.
	now := time.Now()
	launchedAt := now.Add(-1 * time.Hour).Unix()
	readyAt := now.Add(-30 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:               1,
			Status:           db.LaunchStatusRunning,
			LaunchedAt:       &launchedAt,
			AgentReadyAtUnix: &readyAt,
		},
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusRunning},
		JobState:      JobState{HasStartedJob: true},
		InstancePhase: "running:42",
		HeartbeatAge:  30 * time.Second,
		Now:           now,
	})
	if action.Kind == ActionIdleAfterReadyTimeout {
		t.Fatalf("action.Kind = ActionIdleAfterReadyTimeout, must not fire when a job has started")
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

// TestCheckInstance_TerminationIntent_PreservesFailureDetail is the regression
// test for driver-preflight failures being reduced to a generic infrastructure
// diagnosis after the agent self-destructs successfully.
func TestCheckInstance_TerminationIntent_PreservesFailureDetail(t *testing.T) {
	ci := &db.Launch{ID: 42, Status: db.LaunchStatusRunning}
	intent := &instanceintent.Marker{
		TerminalStatus:    db.LaunchStatusFailed,
		TerminationReason: db.TerminationReasonInfraFailure,
		Phase:             phaseDriverTooOld,
		Detail:            "host driver 550.67 (major 550) is below required major 560; terminating before workloads run",
		State:             instanceintent.StateSucceeded,
	}

	action := (&Reconciler{}).checkTerminationIntent(ci, nil, intent, false)
	if got, want := action.StallMessage, intent.Detail; got != want {
		t.Fatalf("StallMessage = %q, want %q", got, want)
	}
}

func TestCheckInstance_TerminationIntent_UnknownProviderStillRequiresDestroy(t *testing.T) {
	action := NewReconciler().CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderErr: errors.New("provider API unavailable"),
		TerminationIntent: &instanceintent.Marker{
			TerminalStatus:    db.LaunchStatusFailed,
			TerminationReason: db.TerminationReasonDiskFull,
		},
		Now: time.Now(),
	})

	if action.Kind != ActionTerminationIntent {
		t.Fatalf("action.Kind = %v, want ActionTerminationIntent", action.Kind)
	}
	if !action.DestroyProvider {
		t.Fatal("unknown provider state was treated as already destroyed")
	}
}

func TestCheckInstance_TerminationIntent_BillableTerminalProviderStillRequiresDestroy(t *testing.T) {
	for _, status := range []string{cloud.ProviderStatusExited, cloud.ProviderStatusStopped, cloud.ProviderStatusError} {
		t.Run(status, func(t *testing.T) {
			action := NewReconciler().CheckInstance(CheckInstanceParams{
				CI:           &db.Launch{ID: 1, Status: db.LaunchStatusRunning, ProviderInstanceID: "billable-1"},
				ProviderInst: &cloud.Instance{ProviderID: "billable-1", Status: status},
				TerminationIntent: &instanceintent.Marker{
					TerminalStatus: db.LaunchStatusFailed,
				},
				Now: time.Now(),
			})
			if action.Kind != ActionTerminationIntent || !action.DestroyProvider {
				t.Fatalf("action = %+v, want termination intent with provider destroy", action)
			}
		})
	}
}

func TestExecuteAction_BillableTerminalProviderDestroyedBeforeLocalTransition(t *testing.T) {
	for _, status := range []string{cloud.ProviderStatusExited, cloud.ProviderStatusError} {
		t.Run(status, func(t *testing.T) {
			database := setupTestDB(t)
			launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai"})
			if err != nil {
				t.Fatalf("CreateLaunch: %v", err)
			}
			if err := db.SetLaunchProviderID(database, launchID, "billable-1"); err != nil {
				t.Fatalf("SetLaunchProviderID: %v", err)
			}
			jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "echo test", "test", "")
			if err != nil {
				t.Fatalf("RecordQueuedWithGPU: %v", err)
			}
			if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
				t.Fatalf("SetJobLaunchID: %v", err)
			}
			ci, _ := db.GetLaunch(database, launchID)
			action := NewReconciler().CheckInstance(CheckInstanceParams{
				CI:           ci,
				ProviderInst: &cloud.Instance{ProviderID: "billable-1", Status: status},
				TerminationIntent: &instanceintent.Marker{
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: db.TerminationReasonInfraFailure,
				},
				Now: time.Now(),
			})
			destroyed := false
			client := &cloud.MockClient{
				ProviderVal: cloud.ProviderVastai,
				DestroyInstanceFunc: func(string) error {
					before, getErr := db.GetLaunch(database, launchID)
					if getErr != nil {
						t.Fatalf("GetLaunch during destroy: %v", getErr)
					}
					if before.Status != db.LaunchStatusRunning {
						t.Fatalf("launch status during provider destroy = %q, want running", before.Status)
					}
					destroyed = true
					return nil
				},
			}
			ExecuteAction(database, client, ci, action)
			if !destroyed {
				t.Fatal("DestroyInstance was not called for billable terminal provider state")
			}
			after, _ := db.GetLaunch(database, launchID)
			if after.Status != db.LaunchStatusFailed {
				t.Fatalf("launch status after destroy = %q, want failed", after.Status)
			}
			job, _ := db.GetJobByID(database, jobID)
			if job.Status != db.StatusQueued || job.LaunchID != nil {
				t.Fatalf("job after transition = status %q launch %v, want queued/unassigned", job.Status, job.LaunchID)
			}
		})
	}
}

func TestCheckInstance_DudProvider_UnknownMarkerObservationsDoNotProveAbsence(t *testing.T) {
	launchedAt := time.Now().Add(-dudVastTimeout - time.Minute).Unix()
	for _, mutate := range []func(*CheckInstanceParams){
		func(p *CheckInstanceParams) { p.InstancePhaseUnknown = true },
		func(p *CheckInstanceParams) { p.BootstrapStageUnknown = true },
	} {
		params := CheckInstanceParams{
			CI:  &db.Launch{ID: 1, Status: db.LaunchStatusRunning, LaunchedAt: &launchedAt},
			Now: time.Now(),
		}
		mutate(&params)
		if action := NewReconciler().CheckInstance(params); action.Kind != ActionNone {
			t.Fatalf("unknown marker observation produced destructive action: %+v", action)
		}
	}
}

func TestCheckInstance_TerminationIntentSucceededDoesNotDestroyProvider(t *testing.T) {
	destroyedAt := time.Now().Add(-7 * time.Hour).Truncate(time.Second)
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		TerminationIntent: &instanceintent.Marker{
			TerminalStatus:         db.LaunchStatusFailed,
			TerminationReason:      db.TerminationReasonJobFailure,
			State:                  instanceintent.StateSucceeded,
			DestroySucceededAtUnix: destroyedAt.Unix(),
			DestroyStartedAtUnix:   destroyedAt.Add(-2 * time.Second).Unix(),
			RequestedAtUnix:        destroyedAt.Add(-4 * time.Second).Unix(),
			DestroyAttempts:        1,
			LastAttemptAtUnix:      destroyedAt.Add(-1 * time.Second).Unix(),
		},
		Now: time.Now(),
	})
	if action.Kind != ActionTerminationIntent {
		t.Fatalf("action.Kind = %d, want ActionTerminationIntent (%d)", action.Kind, ActionTerminationIntent)
	}
	if action.DestroyProvider {
		t.Error("DestroyProvider should be false after the termination intent already recorded destroy success")
	}
	if !action.TerminalAt.Equal(destroyedAt) {
		t.Errorf("TerminalAt = %s, want destroy success time %s", action.TerminalAt, destroyedAt)
	}
	if !action.ResetJobs {
		t.Error("ResetJobs should remain true for failed termination")
	}
}

func TestExecuteActionUsesTerminationIntentDestroyTime(t *testing.T) {
	database := setupTestDB(t)
	destroyedAt := time.Now().Add(-7 * time.Hour).Truncate(time.Second)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}

	reconciled, terminated := ExecuteAction(database, nil, launch, InstanceAction{
		Kind:              ActionTerminationIntent,
		TerminalStatus:    db.LaunchStatusCompleted,
		TerminalAt:        destroyedAt,
		TerminationReason: db.TerminationReasonCompleted,
		AttemptOutcome:    db.AttemptOutcomeCompleted,
	})
	if !reconciled || !terminated {
		t.Fatalf("ExecuteAction = (%v, %v), want (true, true)", reconciled, terminated)
	}

	launch, err = db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch after ExecuteAction: %v", err)
	}
	if launch.EndedAt == nil || *launch.EndedAt != destroyedAt.Unix() {
		t.Fatalf("EndedAt = %v, want %d", launch.EndedAt, destroyedAt.Unix())
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

// Not-found is non-authoritative for initiating termination: vast.ai
// transiently reports not-found for still-booting instances (see
// TestReconcileLaunches_FallbackInstanceNotFound_DoesNotMarkDead), so a
// nil instance with the ErrInstanceNotFound sentinel must not enter the
// provider-dead path. Confirmed-gone instances are reaped by the
// heartbeat/bootstrap watchdogs, which not-found does not defer.
func TestCheckInstance_ConfirmedNotFound_NotProviderDead(t *testing.T) {
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	launchedAt := time.Now().Add(-10 * time.Minute).Unix()
	agentReadyAt := time.Now().Add(-8 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
			LaunchedAt:         &launchedAt,
			AgentReadyAtUnix:   &agentReadyAt,
		},
		ProviderInst: nil,
		ProviderErr:  fmt.Errorf("provider instance test-123: %w", cloud.ErrInstanceNotFound),
		Now:          time.Now(),
	})
	if action.Kind == ActionProviderDead {
		t.Fatalf("not-found must not enter the provider-dead path; got %v", action.Kind)
	}
	if action.TerminalStatus != "" {
		t.Fatalf("not-found must not reach a terminal status; got %q", action.TerminalStatus)
	}
}

// Regression for the never-polled nil: a nil ProviderInst with a nil
// ProviderErr means no lookup happened this pass (no provider ID recorded
// yet, or no client configured for the provider). Rule 7 must not read
// that as provider-dead — doing so reaped launches mid-create (the
// 2026-05-15 16-orphan incident) and healthy launches whose provider
// client was absent from the session's client list.
func TestCheckInstance_NeverPolled_NotProviderDead(t *testing.T) {
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1, // disable hysteresis: a single pass would terminate
	}
	launchedAt := time.Now().Add(-10 * time.Minute).Unix()
	agentReadyAt := time.Now().Add(-8 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123", // provider ID known, but no client polled it
			LaunchedAt:         &launchedAt,
			AgentReadyAtUnix:   &agentReadyAt,
		},
		ProviderInst: nil,
		ProviderErr:  nil,
		Now:          time.Now(),
	})
	if action.Kind == ActionProviderDead {
		t.Fatalf("never-polled launch must not be provider-dead; got %v", action.Kind)
	}
	if action.TerminalStatus != "" {
		t.Fatalf("never-polled launch must not reach a terminal status; got %q", action.TerminalStatus)
	}
}

// Regression for the create window: a `launching` row has no provider ID
// until CreateInstance returns and SetLaunchProviderID runs — a stretch
// that can span minutes of create attempts and offer searches. During
// that window the reconciler sees (nil, nil) and must leave the launch
// alone; the launching-phase timeout (rule 4c) and the bootstrap
// deadline (rule 5) are the adjudicators for a create that never lands.
func TestCheckInstance_LaunchingWithoutProviderID_NotProviderDead(t *testing.T) {
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:        1,
			Status:    db.LaunchStatusLaunching,
			CreatedAt: time.Now().Add(-2 * time.Minute).Unix(),
		},
		ProviderInst: nil,
		ProviderErr:  nil,
		Now:          time.Now(),
	})
	if action.Kind != ActionNone {
		t.Fatalf("launching row without provider ID: action.Kind = %v, want ActionNone", action.Kind)
	}
}

func TestCheckInstance_PauseTolerant_Exited_Preempted(t *testing.T) {
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		probeFailures:      make(map[int64]probeFailureState),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	agentReadyAt := now.Add(-25 * time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
			LaunchedAt:         &launchedAt,
			AgentReadyAtUnix:   &agentReadyAt,
		},
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusExited},
		JobState:      JobState{HasStartedJob: true},
		PauseTolerant: true,
		Now:           now,
	})
	if action.Kind != ActionProviderDead {
		t.Fatalf("action.Kind = %d, want ActionProviderDead (%d)", action.Kind, ActionProviderDead)
	}
	if action.TerminationReason != db.TerminationReasonPreempted {
		t.Errorf("TerminationReason = %q, want %q", action.TerminationReason, db.TerminationReasonPreempted)
	}
	if action.AttemptOutcome != db.AttemptOutcomePreempted {
		t.Errorf("AttemptOutcome = %q, want %q", action.AttemptOutcome, db.AttemptOutcomePreempted)
	}
	if !action.ResetJobs {
		t.Error("expected ResetJobs=true so interruptible work relaunches")
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
	start := time.Now().Add(-3 * time.Minute).Unix()
	end := time.Now().Add(-2 * time.Minute).Unix()
	jobs := []*db.Job{{
		ID:        88,
		Status:    db.StatusQueued,
		StartTime: start,
		EndTime:   &end,
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
	startA := time.Now().Add(-3 * time.Minute).Unix()
	endA := time.Now().Add(-2 * time.Minute).Unix()
	startB := time.Now().Add(-2 * time.Minute).Unix()
	endB := time.Now().Add(-90 * time.Second).Unix()
	jobs := []*db.Job{
		{ID: 88, Status: db.StatusQueued, StartTime: startA, EndTime: &endA},
		{ID: 89, Status: db.StatusQueued, StartTime: startB, EndTime: &endB},
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

// Regression for wi2317: a job canceled at the job level before any
// execution started must NOT count as "started on instance". Otherwise
// rule 4a (provider-status-unavailable) and rule 5a (idle-after-ready)
// silently skip the watchdog and the launch wedges in `running` after
// the provider has destroyed its container.
func TestComputeJobState_CanceledBeforeStartIsNotStarted(t *testing.T) {
	jobs := []*db.Job{{
		ID:        1733,
		Status:    db.StatusCanceled,
		StartTime: 0,
		EndTime:   nil,
	}}
	state := ComputeJobState(jobs, nil)
	if state.HasStartedJob {
		t.Fatal("HasStartedJob = true, want false (canceled with no start_time/end_time)")
	}
}

// A canceled job that did record a start_time must still count as
// started — the cancel happened mid-run. Distinguishes this case from
// the regression above.
func TestComputeJobState_CanceledAfterStartIsStarted(t *testing.T) {
	start := time.Now().Add(-5 * time.Minute).Unix()
	jobs := []*db.Job{{
		ID:        1733,
		Status:    db.StatusCanceled,
		StartTime: start,
	}}
	state := ComputeJobState(jobs, nil)
	if !state.HasStartedJob {
		t.Fatal("HasStartedJob = false, want true (canceled after start_time was set)")
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

// TestCheckInstance_NoAgentReadyDoesNotKill verifies the post-refactor
// behavior: when an instance has never reached agent_ready and the
// provider list is unavailable, the missing-from-list 25m watchdog is
// no longer the kill path. Such instances are now caught by the
// bootstrap_timeout (rule 5) instead. Vast's list-absence is too
// unreliable to use as a kill signal on its own (it produced 25-minute
// false-kill windows on healthy rentals).
func TestCheckInstance_NoAgentReadyDoesNotKill(t *testing.T) {
	launchedAt := time.Now().Add(-26 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		// Probe landed: this test is about provider-API
		// flakiness, not dud-Vast. Without the probe flag set, the
		// dud watchdog would also fire here.
		OnStartProbePresent: true,
		ProviderErr:         fmt.Errorf("provider API timeout"),
		JobState:            JobState{},
		Now:                 time.Now(),
	})
	if action.Kind == ActionEmptyStatusTimeout {
		t.Fatalf("provider absence alone should no longer kill; got ActionEmptyStatusTimeout")
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

// TestCheckInstance_StaleAgentHeartbeatRequiresProbe verifies that the pure
// decision layer only displays heartbeat staleness. The reconciler performs
// the SSH probe and evidence/hysteresis checks before it may terminate.
func TestCheckInstance_StaleAgentHeartbeatRequiresProbe(t *testing.T) {
	// Agent is past the early-life window so the steady-state threshold
	// applies; heartbeat age exceeds it.
	launchedAt := time.Now().Add(-30 * time.Minute).Unix()
	agentReady := time.Now().Add(-25 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			AgentReadyAtUnix:   &agentReady,
			ProviderInstanceID: "test-123",
		},
		// Provider liveness cannot prove the agent is dead, and heartbeat
		// silence can be an R2 path failure rather than a workload failure.
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase: "running:531",
		HeartbeatAge:  heartbeatStaleThreshold + time.Minute,
		JobState:      JobState{HasStartedJob: true},
		Now:           time.Now(),
	})
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %d, want ActionDisplayOnly (%d) — stale heartbeat requires a probe", action.Kind, ActionDisplayOnly)
	}
	if action.DestroyProvider || action.ResetJobs || action.TerminalStatus != "" {
		t.Fatalf("stale heartbeat display action is destructive: %+v", action)
	}
}

// TestCheckInstance_DudProvider_KillsWhenNoProbeAfterTimeout verifies the
// dud-provider watchdog: status=running for ≥dudVastTimeout with no
// OnStart probe in R2 means the container never started, even though
// the provider says it is. The OnStart probe arrival distribution is bimodal
// (within ~60s OR never), so the timeout doesn't false-kill slow-but-
// progressing launches; it cuts a binary host-level failure short
// well below the adaptive bootstrap deadline (often 90+ min).
func TestCheckInstance_DudProvider_KillsWhenNoProbeAfterTimeout(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-dudVastTimeout - time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-dud",
		},
		ProviderInst:        &cloud.Instance{Status: cloud.ProviderStatusRunning},
		OnStartProbePresent: false,
		Now:                 now,
	})
	if action.Kind != ActionEmptyStatusTimeout {
		t.Fatalf("action.Kind = %d, want ActionEmptyStatusTimeout (%q)", action.Kind, action.StallMessage)
	}
	if !strings.Contains(action.StallMessage, "dud provider") {
		t.Errorf("StallMessage = %q, want it to mention 'dud provider'", action.StallMessage)
	}
	if !action.ResetJobs {
		t.Error("ResetJobs should be true so orphaned jobs requeue")
	}
}

// TestCheckInstance_DudProvider_QuietBeforeTimeout: pre-timeout window
// with no probe yet should leave the launch alone.
func TestCheckInstance_DudProvider_QuietBeforeTimeout(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-dudVastTimeout / 2).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-young",
		},
		ProviderInst:        &cloud.Instance{Status: cloud.ProviderStatusRunning},
		OnStartProbePresent: false,
		Now:                 now,
	})
	if action.Kind == ActionEmptyStatusTimeout && strings.Contains(action.StallMessage, "dud provider") {
		t.Fatalf("dud watchdog fired early: %q", action.StallMessage)
	}
}

// TestCheckInstance_DudProvider_QuietWhenProbeLanded verifies the probe-
// present case bypasses the watchdog: OnStart did execute and we
// trust the bootstrap-deadline machinery for any later stall.
func TestCheckInstance_DudProvider_QuietWhenProbeLanded(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-pulled",
		},
		ProviderInst:        &cloud.Instance{Status: cloud.ProviderStatusRunning},
		OnStartProbePresent: true,
		Now:                 now,
	})
	if action.Kind == ActionEmptyStatusTimeout && strings.Contains(action.StallMessage, "dud provider") {
		t.Fatalf("dud watchdog fired despite probe present: %q", action.StallMessage)
	}
}

func TestCheckInstance_DudProvider_QuietWhenHeartbeatTimestampIsCurrent(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	action := NewReconciler().CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-heartbeat",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		Heartbeat:    &HeartbeatSample{Ts: now.Unix()},
		HeartbeatAge: 0,
		Now:          now,
	})

	if action.Kind == ActionEmptyStatusTimeout && strings.Contains(action.StallMessage, "dud provider") {
		t.Fatalf("dud watchdog fired despite a present current-second heartbeat: %q", action.StallMessage)
	}
}

func TestCheckInstance_DudProvider_QuietWhenBootstrapActivitySeen(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-progress",
		},
		ProviderInst:          &cloud.Instance{Status: cloud.ProviderStatusRunning},
		OnStartProbePresent:   false,
		BootstrapActivitySeen: true,
		Now:                   now,
	})
	if action.Kind == ActionEmptyStatusTimeout && strings.Contains(action.StallMessage, "dud provider") {
		t.Fatalf("dud watchdog fired despite historical bootstrap activity: %q", action.StallMessage)
	}
}

// TestCheckInstance_OnStartFailureMarker_Terminates: a terminal OnStart
// install-failure marker (rule 4e) reaps the instance immediately rather than
// waiting the adaptive bootstrap deadline.
func TestCheckInstance_OnStartFailureMarker_Terminates(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	for _, stage := range []string{"apt-failed", "uv-failed", "rclone-failed", "rclone-missing-exit"} {
		t.Run(stage, func(t *testing.T) {
			r := NewReconciler()
			action := r.CheckInstance(CheckInstanceParams{
				CI: &db.Launch{
					ID:                 1,
					Status:             db.LaunchStatusRunning,
					LaunchedAt:         &launchedAt,
					ProviderInstanceID: "test-onstart-failed",
				},
				ProviderInst:        &cloud.Instance{Status: cloud.ProviderStatusRunning},
				OnStartProbePresent: true,
				OnStartStage:        stage,
				Now:                 now,
			})
			if action.Kind != ActionBootstrapStalled {
				t.Fatalf("action.Kind = %d, want ActionBootstrapStalled (%q)", action.Kind, action.StallMessage)
			}
			if !strings.Contains(action.StallMessage, "OnStart failed at "+stage) {
				t.Errorf("StallMessage = %q, want it to mention OnStart failure at %q", action.StallMessage, stage)
			}
			if !action.ResetJobs || !action.DestroyProvider {
				t.Errorf("want ResetJobs and DestroyProvider so orphaned jobs requeue on a fresh offer")
			}
			if action.TerminationReason != db.TerminationReasonInfraFailure {
				t.Errorf("TerminationReason = %q, want infra failure", action.TerminationReason)
			}
		})
	}
}

// TestCheckInstance_OnStartStall_Terminates: an in-progress OnStart stage
// frozen past onStartStallTimeout (rule 4f) — the wi4102 incident: the probe
// landed, apt hung, bootstrap.sh never ran — is reaped without waiting the
// full adaptive deadline.
func TestCheckInstance_OnStartStall_Terminates(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	stageAt := now.Add(-onStartStallTimeout - time.Minute)
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-onstart-hung",
		},
		ProviderInst:          &cloud.Instance{Status: cloud.ProviderStatusRunning},
		OnStartProbePresent:   true,
		OnStartStage:          "apt-installing",
		OnStartStageChangedAt: &stageAt,
		Now:                   now,
	})
	if action.Kind != ActionBootstrapStalled {
		t.Fatalf("action.Kind = %d, want ActionBootstrapStalled (%q)", action.Kind, action.StallMessage)
	}
	if !strings.Contains(action.StallMessage, "OnStart stalled at apt-installing") {
		t.Errorf("StallMessage = %q, want it to mention the stalled OnStart stage", action.StallMessage)
	}
	if !action.ResetJobs || !action.DestroyProvider {
		t.Errorf("want ResetJobs and DestroyProvider so orphaned jobs requeue on a fresh offer")
	}
}

// TestCheckInstance_OnStartStall_NotYetStalled: an in-progress stage that is
// still advancing (changed recently) must not trip rule 4f.
func TestCheckInstance_OnStartStall_NotYetStalled(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	stageAt := now.Add(-onStartStallTimeout / 2)
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-onstart-progressing",
		},
		ProviderInst:          &cloud.Instance{Status: cloud.ProviderStatusRunning},
		OnStartProbePresent:   true,
		OnStartStage:          "rclone-installing",
		OnStartStageChangedAt: &stageAt,
		Now:                   now,
	})
	if strings.Contains(action.StallMessage, "OnStart") {
		t.Fatalf("OnStart watchdog fired while the stage was still advancing: %q", action.StallMessage)
	}
}

// TestCheckInstance_OnStartRestartLoop_Terminates: the wi5105 incident — a
// RunPod OnStart that re-runs from the top on each failure keeps the stage
// marker perpetually fresh, so rule 4f (per-stage stall) never fires. Rule 4g
// anchors on FirstOnStartProbeSeenUnix (set-once) and reaps the looping
// instance regardless of marker churn.
func TestCheckInstance_OnStartRestartLoop_Terminates(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-40 * time.Minute).Unix()
	firstProbe := now.Add(-onStartTotalActiveTimeout - time.Minute).Unix()
	// Marker looks fresh (well under onStartStallTimeout) — the loop just
	// rewrote it — so rule 4f must NOT be what fires here.
	stageAt := now.Add(-30 * time.Second)
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                        1,
			Status:                    db.LaunchStatusLaunching,
			LaunchedAt:                &launchedAt,
			FirstOnStartProbeSeenUnix: &firstProbe,
			ProviderInstanceID:        "test-onstart-loop",
		},
		ProviderInst:          &cloud.Instance{Status: cloud.ProviderStatusRunning},
		OnStartProbePresent:   true,
		OnStartStage:          "onstart-deps-ready",
		OnStartStageChangedAt: &stageAt,
		Now:                   now,
	})
	if action.Kind != ActionBootstrapStalled {
		t.Fatalf("action.Kind = %d, want ActionBootstrapStalled (%q)", action.Kind, action.StallMessage)
	}
	if !strings.Contains(action.StallMessage, "OnStart active") {
		t.Errorf("StallMessage = %q, want it to mention total OnStart-active time", action.StallMessage)
	}
	if !action.ResetJobs || !action.DestroyProvider {
		t.Errorf("want ResetJobs and DestroyProvider so orphaned jobs requeue on a fresh offer")
	}
	if action.TerminationReason != db.TerminationReasonInfraFailure {
		t.Errorf("TerminationReason = %q, want infra failure", action.TerminationReason)
	}
}

// TestCheckInstance_OnStartTotalCap_NotYetExceeded: a fresh, genuinely
// progressing OnStart (probe seen recently, marker advancing) must not trip
// rule 4g — the cap sits well above onStartStallTimeout so slow-but-healthy
// setup is not clipped.
func TestCheckInstance_OnStartTotalCap_NotYetExceeded(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-6 * time.Minute).Unix()
	firstProbe := now.Add(-5 * time.Minute).Unix()
	stageAt := now.Add(-30 * time.Second)
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                        1,
			Status:                    db.LaunchStatusLaunching,
			LaunchedAt:                &launchedAt,
			FirstOnStartProbeSeenUnix: &firstProbe,
			ProviderInstanceID:        "test-onstart-young",
		},
		ProviderInst:          &cloud.Instance{Status: cloud.ProviderStatusRunning},
		OnStartProbePresent:   true,
		OnStartStage:          "rclone-installing",
		OnStartStageChangedAt: &stageAt,
		Now:                   now,
	})
	if strings.Contains(action.StallMessage, "OnStart") {
		t.Fatalf("OnStart watchdog fired on a young, progressing OnStart: %q", action.StallMessage)
	}
}

// TestCheckInstance_OnStartStall_QuietOnceBootstrapStarted: once bootstrap.sh
// has written a stage, the bootstrap-deadline machinery (rules 5/5a) owns the
// adjudication — the OnStart rules must stand down even if a stale OnStart
// marker lingers.
func TestCheckInstance_OnStartStall_QuietOnceBootstrapStarted(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-30 * time.Minute).Unix()
	stageAt := now.Add(-onStartStallTimeout - time.Minute)
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-onstart-handed-off",
		},
		ProviderInst:          &cloud.Instance{Status: cloud.ProviderStatusRunning},
		OnStartProbePresent:   true,
		OnStartStage:          "apt-installing",
		OnStartStageChangedAt: &stageAt,
		BootstrapStage:        "agent_installing",
		Now:                   now,
	})
	if strings.Contains(action.StallMessage, "OnStart") {
		t.Fatalf("OnStart watchdog fired after bootstrap.sh took over: %q", action.StallMessage)
	}
}

func TestCheckInstance_HedgeCull_FiresWhenSiblingIsReady(t *testing.T) {
	cohort := int64(42)
	now := time.Now()
	r := NewReconciler()
	for _, st := range []string{db.LaunchStatusLaunching, db.LaunchStatusRunning} {
		t.Run(st, func(t *testing.T) {
			action := r.CheckInstance(CheckInstanceParams{
				CI: &db.Launch{
					ID:                 1,
					Status:             st,
					CreatedAt:          now.Add(-2 * time.Minute).Unix(),
					ProviderInstanceID: "test-loser",
					HedgeCohortID:      &cohort,
				},
				HedgeCohortHasReadySibling: true,
				Now:                        now,
			})
			if action.Kind != ActionHedgeCull {
				t.Fatalf("status=%s: action.Kind = %d, want ActionHedgeCull", st, action.Kind)
			}
			if action.TerminalStatus != db.LaunchStatusCancelled {
				t.Errorf("TerminalStatus = %q, want %q", action.TerminalStatus, db.LaunchStatusCancelled)
			}
			if !action.DestroyProvider {
				t.Error("DestroyProvider should be true")
			}
		})
	}
}

func TestCheckInstance_HedgeCull_DoesNotCullSurvivor(t *testing.T) {
	cohort := int64(42)
	ready := time.Now().Add(-30 * time.Second).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			AgentReadyAtUnix:   &ready,
			ProviderInstanceID: "test-winner",
			HedgeCohortID:      &cohort,
		},
		HedgeCohortHasReadySibling: true,
		Now:                        time.Now(),
	})
	if action.Kind == ActionHedgeCull {
		t.Fatal("survivor must never be culled even if HedgeCohortHasReadySibling is true")
	}
}

func TestCheckInstance_HedgeCull_NoCohortNoOp(t *testing.T) {
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusLaunching,
			ProviderInstanceID: "test-non-hedge",
		},
		HedgeCohortHasReadySibling: true,
		Now:                        time.Now(),
	})
	if action.Kind == ActionHedgeCull {
		t.Fatal("non-hedge launch must never be culled")
	}
}

// TestEffectiveHeartbeatStaleThreshold_EarlyLifeIsLenient locks in
// the lenient threshold during the first heartbeatEarlyLifeWindow
// after agent_ready_at — a window calibrated to cover the
// memory-intensive uv-sync + HF-download burst right after the
// agent pulls its first job. Once past that window, threshold
// tightens to the steady-state value where the sidecar should
// heartbeat reliably.
func TestEffectiveHeartbeatStaleThreshold_EarlyLifeIsLenient(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name     string
		readyAge time.Duration
		readyAt  *int64
		want     time.Duration
	}{
		{"no agent_ready_at", 0, nil, heartbeatStaleThreshold},
		{"early-life: just reached ready", 30 * time.Second, nil, heartbeatStaleThresholdEarlyLife},
		{"early-life: at boundary", heartbeatEarlyLifeWindow - time.Second, nil, heartbeatStaleThresholdEarlyLife},
		{"steady-state: past window", heartbeatEarlyLifeWindow + time.Minute, nil, heartbeatStaleThreshold},
		{"steady-state: long-running", time.Hour, nil, heartbeatStaleThreshold},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var ready *int64
			if tc.readyAge > 0 {
				ts := now.Add(-tc.readyAge).Unix()
				ready = &ts
			} else {
				ready = tc.readyAt
			}
			got := effectiveHeartbeatStaleThreshold(ready, now)
			if got != tc.want {
				t.Fatalf("readyAge=%s threshold=%s, want %s", tc.readyAge, got, tc.want)
			}
		})
	}
}

// TestJobStartedOnInstance_EndTimeAloneNotSufficient locks in that
// jobStartedOnInstance does NOT treat end_time alone as evidence of
// execution. The supersede paths (tryPlaceOntoExistingInstances on
// dep-blocked or producer-missing jobs) stamp end_time on canceled
// attempts that the agent never saw; if those counted as "started",
// HasStartedJob would silence the bootstrap-stall and idle-after-ready
// watchdogs forever. wi2349 hit this with 360+ canceled-with-end_time
// attempts on wj1756 — instance ran 95 min past its 1h32m bootstrap
// deadline without rule 5 firing.
func TestJobStartedOnInstance_EndTimeAloneNotSufficient(t *testing.T) {
	end := int64(1000)
	canceled := &db.Job{ID: 1, StartTime: 0, EndTime: &end}
	if jobStartedOnInstance(canceled) {
		t.Fatal("end_time set without start_time must not signal a run; that is how supersede chains thrash")
	}
	started := &db.Job{ID: 2, StartTime: 100, EndTime: &end}
	if !jobStartedOnInstance(started) {
		t.Fatal("start_time set must signal a run")
	}
	queued := &db.Job{ID: 3}
	if jobStartedOnInstance(queued) {
		t.Fatal("a queued job with no times must not signal a run")
	}
}

// TestCheckInstance_LaunchingPhaseTimeout verifies that an instance stuck
// in `launching` (provider never reported status=running, so no
// BootstrapOrigin) is killed once launchingPhaseTimeout elapses since
// CreatedAt — and not before. Calibration: ~1500 historical launches
// show pre-running provisioning is fast-or-never, with observed max
// 6.2 min and p99 ≤ 2 min, so 12 min has ~2× margin over the worst
// recorded case.
func TestCheckInstance_LaunchingPhaseTimeout(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		age        time.Duration
		expectKill bool
	}{
		{"under threshold", launchingPhaseTimeout - time.Minute, false},
		{"at threshold", launchingPhaseTimeout, true},
		{"over threshold", launchingPhaseTimeout + 5*time.Minute, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			created := now.Add(-tc.age).Unix()
			r := NewReconciler()
			action := r.CheckInstance(CheckInstanceParams{
				CI: &db.Launch{
					ID:                 1,
					Status:             db.LaunchStatusLaunching,
					CreatedAt:          created,
					ProviderInstanceID: "test-123",
				},
				// ProviderInst nil simulates the real failure mode wi2350
				// hit: Vast neither in the batch list nor returning a
				// ShowInstance hit. With a non-nil ProviderInst the
				// empty-status rule 4 fires first.
				ProviderInst: nil,
				Now:          now,
			})
			killed := action.Kind == ActionEmptyStatusTimeout && strings.Contains(action.StallMessage, "launching phase exceeded")
			if killed != tc.expectKill {
				t.Fatalf("age=%s killed_by_4c=%v, want killed_by_4c=%v (action=%d, msg=%q)",
					tc.age, killed, tc.expectKill, action.Kind, action.StallMessage)
			}
		})
	}
}

// TestCheckInstance_LaunchingPhaseTimeoutInertOnceRunning verifies that
// rule 4c is inert once the provider has reported status=running
// (LaunchedAt is stamped, BootstrapOrigin is non-nil) — the adaptive
// bootstrap watchdog should take over from there.
func TestCheckInstance_LaunchingPhaseTimeoutInertOnceRunning(t *testing.T) {
	now := time.Now()
	created := now.Add(-30 * time.Minute).Unix()
	launchedAt := now.Add(-29 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusLaunching,
			CreatedAt:          created,
			LaunchedAt:         &launchedAt,
			ProviderInstanceID: "test-123",
		},
		ProviderInst: &cloud.Instance{Status: cloud.ProviderStatusRunning},
		Now:          now,
	})
	if action.Kind == ActionEmptyStatusTimeout && strings.Contains(action.StallMessage, "launching phase exceeded") {
		t.Fatalf("rule 4c should not fire once LaunchedAt is set (BootstrapOrigin non-nil); got: %q", action.StallMessage)
	}
}

// TestCheckInstance_StaleHeartbeatYieldsToPaused verifies that the
// stale-heartbeat watchdog defers to the paused-provider rule: a
// paused container stops writing heartbeats, so without this carve-out
// rule 4a would always fire first and we'd destroy paused-but-
// recoverable instances.
func TestCheckInstance_StaleHeartbeatYieldsToPaused(t *testing.T) {
	launchedAt := time.Now().Add(-30 * time.Minute).Unix()
	agentReady := time.Now().Add(-25 * time.Minute).Unix()
	for _, status := range []string{cloud.ProviderStatusStopped, cloud.ProviderStatusOffline} {
		t.Run(status, func(t *testing.T) {
			r := NewReconciler()
			action := r.CheckInstance(CheckInstanceParams{
				CI: &db.Launch{
					ID:                 1,
					Status:             db.LaunchStatusRunning,
					LaunchedAt:         &launchedAt,
					AgentReadyAtUnix:   &agentReady,
					CreatedAt:          launchedAt,
					ProviderInstanceID: "test-123",
				},
				ProviderInst:  &cloud.Instance{Status: status},
				HeartbeatAge:  10 * time.Minute,
				JobState:      JobState{HasStartedJob: true},
				PauseTolerant: true,
				Now:           time.Now(),
			})
			if action.Kind == ActionEmptyStatusTimeout {
				t.Fatalf("provider status %q with stale heartbeat: rule 4a fired (terminating) instead of yielding to rule 4a-pause", status)
			}
			if action.Kind != ActionPause {
				t.Fatalf("provider status %q: action.Kind = %d, want ActionPause", status, action.Kind)
			}
		})
	}
}

// TestCheckInstance_FreshHeartbeatLeavesAlone verifies that a fresh
// heartbeat keeps the instance running even if the provider list is
// noisy.
func TestCheckInstance_FreshHeartbeatLeavesAlone(t *testing.T) {
	launchedAt := time.Now().Add(-26 * time.Minute).Unix()
	agentReady := time.Now().Add(-25 * time.Minute).Unix()
	r := NewReconciler()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			AgentReadyAtUnix:   &agentReady,
			ProviderInstanceID: "test-123",
		},
		ProviderErr:   fmt.Errorf("provider instance test-123 missing from batch list"),
		InstancePhase: "running:531",
		HeartbeatAge:  30 * time.Second,
		JobState:      JobState{HasStartedJob: true},
		Now:           time.Now(),
	})
	if action.Kind == ActionEmptyStatusTimeout {
		t.Fatalf("fresh heartbeat should keep instance alive even if provider list misses it")
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
	agentReadyAt := now.Add(-6*time.Hour - 55*time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
			CreatedAt:          launchedAt,
			LaunchedAt:         &launchedAt,
			AgentReadyAtUnix:   &agentReadyAt,
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
	if action.AttemptOutcome != db.AttemptOutcomePreempted {
		t.Errorf("AttemptOutcome = %q, want %q", action.AttemptOutcome, db.AttemptOutcomePreempted)
	}
	if !action.ResetJobs {
		t.Error("expected ResetJobs=true so relaunch picks a fresh offer")
	}
	if !action.DestroyProvider {
		t.Error("expected DestroyProvider=true")
	}
}

func TestCheckInstance_PauseTolerant_RecentPauseUsesStatusTransition(t *testing.T) {
	r := NewReconciler()
	now := time.Now()
	launchedAt := now.Add(-7 * time.Hour).Unix()
	lastChange := now.Add(-10 * time.Minute)
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
			CreatedAt:          launchedAt,
			LaunchedAt:         &launchedAt,
		},
		ProviderInst:               &cloud.Instance{Status: cloud.ProviderStatusStopped},
		PauseTolerant:              true,
		LastProviderStatusChangeAt: &lastChange,
		Now:                        now,
	})
	if action.Kind != ActionPause {
		t.Fatalf("action.Kind = %d, want ActionPause (%d)", action.Kind, ActionPause)
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
		HeartbeatAge: heartbeatStaleThreshold + time.Minute,
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
	launchedAt := time.Now().Add(-50 * time.Minute).Unix()
	phaseStart := time.Now().Add(-46 * time.Minute)
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
	launchedAt := time.Now().Add(-35 * time.Minute).Unix()
	phaseStart := time.Now().Add(-30 * time.Minute)
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

func TestCheckInstance_SetupStall_ToleratesLongQuietInstall(t *testing.T) {
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
	if action.Kind == ActionSetupStalled {
		t.Fatal("26 minute setup phase should not be terminated by default")
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

// TestCheckInstance_SetupStall_NilPhaseChangedAt_DoesNotTerminate is the
// regression test for wi7023: launch age is not setup age, so an unknown setup
// start cannot support a destructive phase-stall decision.
func TestCheckInstance_SetupStall_NilPhaseChangedAt_DoesNotTerminate(t *testing.T) {
	launchedAt := time.Now().Add(-50 * time.Minute).Unix()
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
		// PhaseChangedAt is nil: launch time must not substitute for setup start.
		JobState: JobState{HasStartedJob: true},
		Now:      time.Now(),
	})
	if action.Kind == ActionSetupStalled {
		t.Fatal("unknown setup start must not trigger destructive setup-stall handling")
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

func TestCheckInstance_SetupStall_QueuedJobNoPhaseChangedAt_DoesNotTerminate(t *testing.T) {
	launchedAt := time.Now().Add(-50 * time.Minute).Unix()
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
		// PhaseChangedAt intentionally missing; queued state supplies no timing evidence.
		JobState: JobState{HasStartedJob: false},
		Now:      time.Now(),
	})
	if action.Kind == ActionSetupStalled {
		t.Fatal("unknown setup start must not trigger destructive setup-stall handling")
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
	progressChangedAt := time.Now().Add(-65 * time.Minute)
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
		ProviderInst:         &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:        "running:531",
		HeartbeatAge:         30 * time.Second, // liveness does not prove task progress
		JobProgressID:        531,
		JobProgressPct:       25,
		JobProgressChangedAt: &progressChangedAt,
		JobState:             JobState{HasStartedJob: true},
		Now:                  time.Now(),
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

func TestCheckInstance_ProviderUnknownDefersEveryDestructiveLivenessAction(t *testing.T) {
	kinds := []InstanceActionKind{
		ActionEmptyStatusTimeout,
		ActionBootstrapStalled,
		ActionSetupStalled,
		ActionRunningStalled,
		ActionIdleAfterReadyTimeout,
		ActionTerminalLivePhase,
		ActionStaleHeartbeat,
	}
	providerErr := errors.New("provider status unavailable")
	for _, kind := range kinds {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			params := CheckInstanceParams{
				CI:                       &db.Launch{InstanceType: cloud.InstanceTypeInterruptible},
				ProviderErr:              providerErr,
				ProviderStatusUnknownFor: time.Hour,
			}
			if !isDestructiveLivenessAction(kind) {
				t.Fatalf("kind %v missing from destructive liveness policy", kind)
			}
			deferred, ok := deferTerminationForUnknownStatus(params, "test watchdog")
			if !ok || deferred.Kind != ActionDisplayOnly {
				t.Fatalf("kind %v deferral = (%+v, %v), want display-only", kind, deferred, ok)
			}

			params.CI.InstanceType = cloud.InstanceTypeOnDemand
			if _, ok := deferTerminationForUnknownStatus(params, "test watchdog"); ok {
				t.Fatalf("kind %v deferred for on-demand launch", kind)
			}
			params.CI.InstanceType = cloud.InstanceTypeInterruptible
			params.ProviderStatusUnknownFor = stalePauseTimeout
			if _, ok := deferTerminationForUnknownStatus(params, "test watchdog"); ok {
				t.Fatalf("kind %v deferred after bounded unknown window", kind)
			}
		})
	}

	for _, kind := range []InstanceActionKind{ActionTerminationIntent, ActionProviderDead, ActionSelfDestructFailed, ActionGraceExpired, ActionDonorComplete, ActionHedgeCull} {
		if isDestructiveLivenessAction(kind) {
			t.Fatalf("positive/non-liveness action %v incorrectly classified as liveness-derived", kind)
		}
	}
}

func TestCheckInstance_ProviderUnknownDefersSetupStallUntilBound(t *testing.T) {
	now := time.Now()
	launchedAt := now.Add(-2 * time.Hour).Unix()
	phaseChangedAt := now.Add(-90 * time.Minute)
	providerErr := errors.New("provider status unavailable")
	params := CheckInstanceParams{
		CI: &db.Launch{
			ID:           1,
			Status:       db.LaunchStatusRunning,
			InstanceType: cloud.InstanceTypeInterruptible,
			LaunchedAt:   &launchedAt,
		},
		ProviderErr:              providerErr,
		ProviderStatusUnknownFor: time.Hour,
		InstancePhase:            "setup:531",
		PhaseChangedAt:           &phaseChangedAt,
		JobState:                 JobState{HasStartedJob: true},
		Now:                      now,
	}
	if action := NewReconciler().CheckInstance(params); action.Kind != ActionDisplayOnly {
		t.Fatalf("interruptible action = %+v, want bounded display-only deferral", action)
	}

	params.CI.InstanceType = cloud.InstanceTypeOnDemand
	if action := NewReconciler().CheckInstance(params); action.Kind != ActionSetupStalled {
		t.Fatalf("on-demand action = %+v, want setup-stalled adjudication", action)
	}

	params.CI.InstanceType = cloud.InstanceTypeInterruptible
	params.ProviderStatusUnknownFor = stalePauseTimeout
	if action := NewReconciler().CheckInstance(params); action.Kind != ActionSetupStalled {
		t.Fatalf("bounded interruptible action = %+v, want setup-stalled adjudication", action)
	}
}

func TestCheckInstance_RunningStall_WarnsBeforeTermination(t *testing.T) {
	launchedAt := time.Now().Add(-45 * time.Minute).Unix()
	progressChangedAt := time.Now().Add(-25 * time.Minute)
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
		ProviderInst:         &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:        "running:531",
		JobProgressID:        531,
		JobProgressPct:       25,
		JobProgressChangedAt: &progressChangedAt,
		JobState:             JobState{HasStartedJob: true},
		Now:                  time.Now(),
	})
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %d, want ActionDisplayOnly (%d) for warn phase", action.Kind, ActionDisplayOnly)
	}
	if action.StallMessage == "" {
		t.Fatal("expected non-empty stall message")
	}
}

func TestCheckInstance_RunningStall_NoStructuredProgressNoAction(t *testing.T) {
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
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase: "running:531",
		HeartbeatAge:  65 * time.Minute,
		JobState:      JobState{HasStartedJob: true},
		Now:           time.Now(),
	})
	if action.Kind == ActionRunningStalled {
		t.Fatal("activity/liveness evidence alone must not trigger a task-progress stall")
	}
}

func TestCheckInstance_RunningStall_FreshStructuredProgressNoAction(t *testing.T) {
	launchedAt := time.Now().Add(-60 * time.Minute).Unix()
	progressChangedAt := time.Now().Add(-5 * time.Minute)
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
		ProviderInst:         &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase:        "running:531",
		HeartbeatAge:         70 * time.Minute,
		JobProgressID:        531,
		JobProgressPct:       26,
		JobProgressChangedAt: &progressChangedAt,
		JobState:             JobState{HasStartedJob: true},
		Now:                  time.Now(),
	})
	if action.Kind == ActionRunningStalled {
		t.Fatal("should not trigger running stall when structured progress is fresh")
	}
}

func TestCheckInstance_AgentExitedHeartbeatTerminates(t *testing.T) {
	launchedAt := time.Now().Add(-10 * time.Minute).Unix()
	agentAlive := false
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
		InstancePhase: "running:531",
		Heartbeat:     &HeartbeatSample{Ts: time.Now().Unix(), Phase: "running:531", AgentPID: 1234, AgentAlive: &agentAlive},
		HeartbeatAge:  30 * time.Second,
		JobState:      JobState{HasStartedJob: true},
		Now:           time.Now(),
	})
	if action.Kind != ActionRunningStalled {
		t.Fatalf("action.Kind = %d, want ActionRunningStalled (%d)", action.Kind, ActionRunningStalled)
	}
	if action.TerminationReason != db.TerminationReasonInfraFailure {
		t.Fatalf("termination reason = %q, want %q", action.TerminationReason, db.TerminationReasonInfraFailure)
	}
	if !action.DestroyProvider || !action.ResetJobs {
		t.Fatal("expected agent-exited heartbeat to destroy provider and reset jobs")
	}
}

func TestCheckInstance_RunningStall_RepeatedActivityWithoutProgressNoActionWhenUninstrumented(t *testing.T) {
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
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusRunning},
		InstancePhase: "running:531",
		HeartbeatAge:  0,
		JobState:      JobState{HasStartedJob: true},
		Now:           time.Now(),
	})
	if action.Kind == ActionRunningStalled {
		t.Fatal("uninstrumented jobs should remain bounded by runtime/spend limits")
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
	// On-demand Vast "offline" is provider-dead evidence, not a resumable
	// interruptible preemption. It should skip the paused state.
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	now := time.Now()
	launchedAt := now.Add(-7 * time.Hour).Unix()
	agentReadyAt := now.Add(-6*time.Hour - 55*time.Minute).Unix()
	action := r.CheckInstance(CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			ProviderInstanceID: "test-123",
			CreatedAt:          launchedAt,
			LaunchedAt:         &launchedAt,
			AgentReadyAtUnix:   &agentReadyAt,
		},
		ProviderInst:  &cloud.Instance{Status: cloud.ProviderStatusOffline},
		JobState:      JobState{HasStartedJob: true},
		PauseTolerant: false,
		Now:           now,
	})
	if action.Kind != ActionProviderDead {
		t.Fatalf("action.Kind = %d, want ActionProviderDead (%d)", action.Kind, ActionProviderDead)
	}
	if action.TerminationReason != db.TerminationReasonProviderFailure {
		t.Errorf("TerminationReason = %q, want %q", action.TerminationReason, db.TerminationReasonProviderFailure)
	}
	if action.AttemptOutcome != db.AttemptOutcomeOrphaned {
		t.Errorf("AttemptOutcome = %q, want %q", action.AttemptOutcome, db.AttemptOutcomeOrphaned)
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
		// OnStart already ran (instance was running for most of the
		// 30 min), so the dud-Vast watchdog is not the rule under test.
		OnStartProbePresent:        true,
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
	if action.AttemptOutcome != db.AttemptOutcomePreempted {
		t.Errorf("AttemptOutcome = %q, want %q", action.AttemptOutcome, db.AttemptOutcomePreempted)
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

// --- Provider-status-unknown deferral for interruptible launches ---
//
// When status polling fails (ProviderInst nil, ProviderErr set), an
// interruptible launch that has stopped heartbeating may simply be outbid
// (offline, recoverable), not dead. The watchdogs must defer termination
// until either a poll succeeds or the unknown window exceeds
// stalePauseTimeout. See the wj3741 incident: 16 consecutive orphaned
// attempts because "vastai show instances" timeouts were conflated with
// instance death.

func unknownStatusStaleHeartbeatParams(instanceType string, unknownFor time.Duration) CheckInstanceParams {
	launchedAt := time.Now().Add(-30 * time.Minute).Unix()
	agentReady := time.Now().Add(-25 * time.Minute).Unix()
	return CheckInstanceParams{
		CI: &db.Launch{
			ID:                 1,
			Status:             db.LaunchStatusRunning,
			LaunchedAt:         &launchedAt,
			AgentReadyAtUnix:   &agentReady,
			ProviderInstanceID: "test-123",
			InstanceType:       instanceType,
		},
		ProviderErr:              fmt.Errorf("vastai show instances timed out after 30s"),
		ProviderStatusUnknownFor: unknownFor,
		InstancePhase:            "running:531",
		HeartbeatAge:             heartbeatStaleThreshold + time.Minute,
		JobState:                 JobState{HasStartedJob: true},
		PauseTolerant:            true,
		Now:                      time.Now(),
	}
}

func TestCheckInstance_UnknownStatus_InterruptibleStaleHeartbeatDefers(t *testing.T) {
	r := NewReconciler()
	action := r.CheckInstance(unknownStatusStaleHeartbeatParams(cloud.InstanceTypeInterruptible, 5*time.Minute))
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %v, want ActionDisplayOnly (defer) — got message %q", action.Kind, action.StallMessage)
	}
	if action.StallMessage != "heartbeat stale" {
		t.Errorf("StallMessage = %q, want heartbeat stale", action.StallMessage)
	}
}

func TestCheckInstance_UnknownStatus_OnDemandStaleHeartbeatRequiresProbe(t *testing.T) {
	for _, instanceType := range []string{cloud.InstanceTypeOnDemand, ""} {
		t.Run("type="+instanceType, func(t *testing.T) {
			r := NewReconciler()
			action := r.CheckInstance(unknownStatusStaleHeartbeatParams(instanceType, 5*time.Minute))
			if action.Kind != ActionDisplayOnly {
				t.Fatalf("action.Kind = %v, want ActionDisplayOnly — heartbeat silence alone is not terminal evidence", action.Kind)
			}
			if action.DestroyProvider || action.ResetJobs || action.TerminalStatus != "" {
				t.Fatalf("stale heartbeat display action is destructive: %+v", action)
			}
		})
	}
}

func TestCheckInstance_UnknownStatus_InterruptibleStillRequiresProbePastBound(t *testing.T) {
	r := NewReconciler()
	action := r.CheckInstance(unknownStatusStaleHeartbeatParams(cloud.InstanceTypeInterruptible, stalePauseTimeout+time.Minute))
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %v, want ActionDisplayOnly — heartbeat silence remains non-terminal evidence", action.Kind)
	}
}

// A definitive provider not-found is an answer, not a failed poll: the
// deferral must not delay requeue of a genuinely destroyed interruptible
// instance's jobs.
func TestCheckInstance_NotFoundStatus_InterruptibleTerminates(t *testing.T) {
	r := NewReconciler()
	params := unknownStatusStaleHeartbeatParams(cloud.InstanceTypeInterruptible, 5*time.Minute)
	params.ProviderErr = fmt.Errorf("provider instance test-123: %w", cloud.ErrInstanceNotFound)
	action := r.CheckInstance(params)
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %v, want ActionDisplayOnly — transient not-found plus heartbeat silence is not terminal evidence", action.Kind)
	}
}

// TestCheckInstance_KnownPausedStatus_InterruptibleStillPauses verifies the
// deferral does not disturb the existing pause-wait path: when the poll
// SUCCEEDS and reports a recoverable paused status, rule 4a-pause still
// marks the launch paused.
func TestCheckInstance_KnownPausedStatus_InterruptibleStillPauses(t *testing.T) {
	params := unknownStatusStaleHeartbeatParams(cloud.InstanceTypeInterruptible, 0)
	params.ProviderErr = nil
	params.ProviderInst = &cloud.Instance{Status: cloud.ProviderStatusOffline}
	params.CI.CreatedAt = *params.CI.LaunchedAt
	r := NewReconciler()
	action := r.CheckInstance(params)
	if action.Kind != ActionPause {
		t.Fatalf("action.Kind = %v, want ActionPause — successful poll showing paused status keeps pause-wait behavior", action.Kind)
	}
}

func TestCheckInstance_UnknownStatus_InterruptibleBootstrapTimeoutDefers(t *testing.T) {
	launchedAt := time.Now().Add(-25 * time.Minute).Unix()
	expiredDeadline := time.Now().Add(-5 * time.Minute).Unix()
	params := CheckInstanceParams{
		CI: &db.Launch{
			ID:                    1,
			Status:                db.LaunchStatusRunning,
			LaunchedAt:            &launchedAt,
			BootstrapDeadlineUnix: &expiredDeadline,
			ProviderInstanceID:    "test-123",
			InstanceType:          cloud.InstanceTypeInterruptible,
		},
		ProviderErr:              fmt.Errorf("vastai show instances timed out after 30s"),
		ProviderStatusUnknownFor: 5 * time.Minute,
		OnStartProbePresent:      true,
		JobState:                 JobState{HasStartedJob: false, AllJobsTerminal: true},
		Now:                      time.Now(),
	}
	r := NewReconciler()
	action := r.CheckInstance(params)
	if action.Kind != ActionDisplayOnly {
		t.Fatalf("action.Kind = %v, want ActionDisplayOnly (defer) — got message %q", action.Kind, action.StallMessage)
	}
	if !strings.Contains(action.StallMessage, "provider status unknown") {
		t.Errorf("StallMessage = %q, want unknown-status deferral message", action.StallMessage)
	}

	// Same scenario with a non-interruptible launch terminates as today.
	params.CI.InstanceType = cloud.InstanceTypeOnDemand
	action = NewReconciler().CheckInstance(params)
	if action.Kind != ActionBootstrapStalled {
		t.Fatalf("action.Kind = %v, want ActionBootstrapStalled for on-demand launch", action.Kind)
	}
}

// TestReconcilerNoteProviderStatusPoll verifies the unknown-window tracking:
// duration grows across consecutive failed polls and resets on any pass
// where status is known.
func TestReconcilerNoteProviderStatusPoll(t *testing.T) {
	r := NewReconciler()
	t0 := time.Unix(1_000_000, 0)

	if d := r.noteProviderStatusPoll(1, true, t0); d != 0 {
		t.Fatalf("first unknown pass duration = %s, want 0", d)
	}
	if d := r.noteProviderStatusPoll(1, true, t0.Add(3*time.Minute)); d != 3*time.Minute {
		t.Fatalf("second unknown pass duration = %s, want 3m", d)
	}
	// Independent launches track independently.
	if d := r.noteProviderStatusPoll(2, true, t0.Add(3*time.Minute)); d != 0 {
		t.Fatalf("other launch duration = %s, want 0", d)
	}
	// A known-status pass clears the window.
	if d := r.noteProviderStatusPoll(1, false, t0.Add(4*time.Minute)); d != 0 {
		t.Fatalf("known-status pass duration = %s, want 0", d)
	}
	if d := r.noteProviderStatusPoll(1, true, t0.Add(5*time.Minute)); d != 0 {
		t.Fatalf("post-reset unknown pass duration = %s, want 0 (fresh window)", d)
	}
}
