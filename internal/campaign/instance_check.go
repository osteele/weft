package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// InstanceActionKind describes what reconciliation action should be taken.
type InstanceActionKind int

const (
	ActionNone               InstanceActionKind = iota
	ActionDisplayOnly                           // stall message only, no state change
	ActionGraceExpired                          // grace deadline passed -> destroy + fail
	ActionDonorComplete                         // donor ready marker -> destroy + complete
	ActionTerminationIntent                     // R2 intent -> destroy + mark terminal
	ActionEmptyStatusTimeout                    // stuck with no provider status -> fail
	ActionBootstrapStalled                      // no bootstrap progress -> fail
	ActionBootstrapComplete                     // bootstrap stalled but R2 completion marker found -> complete
	ActionProviderDead                          // provider says dead (with hysteresis) -> fail/complete
	ActionSelfDestructFailed                    // all jobs done, instance lingering -> complete
)

// InstanceAction describes what reconciliation action to take for a cloud instance.
type InstanceAction struct {
	Kind              InstanceActionKind
	TerminalStatus    string // db status to set (failed, completed, "")
	TerminationReason string // reason string for DB
	StallMessage      string // human-readable message
	DestroyProvider   bool   // whether to destroy the provider instance
	ResetJobs         bool   // whether to reset jobs to unplaced
	AttemptOutcome    string // attempt outcome when resetting/closing
}

// JobState summarizes the aggregate state of jobs associated with a cloud instance.
type JobState struct {
	HasStartedJob   bool
	AllJobsTerminal bool
	LatestJobEnd    int64 // unix timestamp of latest job end, 0 if none
}

// ComputeJobState computes aggregate job state from a list of jobs.
func ComputeJobState(jobs []*db.Job) JobState {
	s := JobState{AllJobsTerminal: true}
	for _, j := range jobs {
		if j.Status != db.StatusQueued {
			s.HasStartedJob = true
		}
		if !IsJobTerminal(j.Status) {
			s.AllJobsTerminal = false
		}
		if j.EndTime != nil && *j.EndTime > s.LatestJobEnd {
			s.LatestJobEnd = *j.EndTime
		}
	}
	return s
}

// CheckInstanceParams holds all the pre-fetched state needed to evaluate an instance.
type CheckInstanceParams struct {
	CI             *db.Launch
	ProviderInst   *cloud.Instance
	ProviderErr    error
	R2Client       *r2.Client
	JobState       JobState
	InstancePhase  string // from R2
	BootstrapStage string // from R2
	HeartbeatAge   time.Duration
	Now            time.Time

	// TerminationIntent from R2 or DB (pre-fetched by caller)
	TerminationIntent *instanceintent.Marker

	// BootstrapSurvival holds adaptive bootstrap thresholds from historical
	// survival analysis. Nil means use package defaults.
	BootstrapSurvival *db.BootstrapSurvival
}

// CheckInstance evaluates what reconciliation action should be taken for a
// single cloud instance. This is the single source of truth for all instance
// reconciliation decisions. It does not mutate state, but may perform read-only
// R2 queries (donor ready check, completion marker).
//
// Hysteresis state (dead confirmation, probe failures) is tracked on the
// Reconciler. For WatchInstance, use a per-goroutine Reconciler.
func (r *Reconciler) CheckInstance(p CheckInstanceParams) InstanceAction {
	ci := p.CI
	if ci == nil {
		return InstanceAction{Kind: ActionNone}
	}

	// 1. Grace expiry: deadline has passed
	if ci.Status == db.LaunchStatusGrace && ci.GraceDeadline != nil && p.Now.Unix() > *ci.GraceDeadline {
		return InstanceAction{
			Kind:              ActionGraceExpired,
			TerminalStatus:    db.LaunchStatusFailed,
			TerminationReason: db.TerminationReasonJobFailure,
			StallMessage:      "grace period expired — terminating instance",
			DestroyProvider:   true,
			ResetJobs:         true,
			AttemptOutcome:    db.AttemptOutcomeOrphaned,
		}
	}

	// 2. Donor completion
	if ci.InstanceRole == "donor" && ci.Status == db.LaunchStatusRunning && p.R2Client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		exists, _ := p.R2Client.ObjectExists(ctx, r2keys.DonorReady(ci.ID))
		cancel()
		if exists {
			return InstanceAction{
				Kind:              ActionDonorComplete,
				TerminalStatus:    db.LaunchStatusCompleted,
				TerminationReason: db.TerminationReasonCompleted,
				DestroyProvider:   true,
			}
		}
	}

	// 3. Termination intent from R2/DB
	if p.TerminationIntent != nil {
		if action := r.checkTerminationIntent(ci, p.ProviderInst, p.TerminationIntent); action.Kind != ActionNone {
			return action
		}
	}

	// 4. Empty provider status timeout
	if p.ProviderInst != nil && p.ProviderInst.Status == "" && ci.LaunchedAt != nil {
		age := p.Now.Sub(time.Unix(*ci.LaunchedAt, 0))
		if age > maxEmptyStatusTime {
			return InstanceAction{
				Kind:              ActionEmptyStatusTimeout,
				TerminalStatus:    db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonInfraFailure,
				StallMessage:      "provider instance has empty status — terminating",
				DestroyProvider:   true,
				ResetJobs:         true,
				AttemptOutcome:    db.AttemptOutcomeOrphaned,
			}
		}
	}

	// 4b. Stale pre-running status: provider allocated but never reached "running".
	// Covers "created", "loading", and any other non-running, non-terminal status.
	// Skip when IntendedStatus already signals termination — step 8 catches that faster.
	if p.ProviderInst != nil && p.ProviderInst.Status != "running" && p.ProviderInst.Status != "" &&
		!isProviderTerminal(p.ProviderInst) && ci.LaunchedAt != nil {
		age := p.Now.Sub(time.Unix(*ci.LaunchedAt, 0))
		if age > maxPreRunningStatusTime {
			return InstanceAction{
				Kind:              ActionEmptyStatusTimeout,
				TerminalStatus:    db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonInfraFailure,
				StallMessage:      fmt.Sprintf("provider instance stuck in %q status — terminating", p.ProviderInst.Status),
				DestroyProvider:   true,
				ResetJobs:         true,
				AttemptOutcome:    db.AttemptOutcomeOrphaned,
			}
		}
	}

	// 5. Bootstrap stall: instance running but no job progress after timeout
	if ci.Status == db.LaunchStatusRunning && ci.LaunchedAt != nil && !p.JobState.HasStartedJob && p.InstancePhase == "" && p.BootstrapStage != bootstrapStageReady {
		elapsed := p.Now.Sub(time.Unix(*ci.LaunchedAt, 0))

		warnTimeout := bootstrapWarnTimeout
		termTimeout := bootstrapTerminateTimeout
		if p.BootstrapSurvival != nil {
			warnTimeout = p.BootstrapSurvival.WarnAfter
			termTimeout = p.BootstrapSurvival.TerminateAfter
		}

		if elapsed >= warnTimeout {
			// Check R2 completion marker only past the warn threshold,
			// to avoid an R2 call on every reconciliation tick.
			if hasR2CompletionMarker(p.R2Client, ci.ID) {
				return InstanceAction{
					Kind:            ActionBootstrapComplete,
					TerminalStatus:  db.LaunchStatusCompleted,
					StallMessage:    "instance completed but self-destruct failed — cleaning up",
					DestroyProvider: true,
				}
			}
			if elapsed >= termTimeout {
				return InstanceAction{
					Kind:              ActionBootstrapStalled,
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: db.TerminationReasonBootstrapTimeout,
					StallMessage:      fmt.Sprintf("bootstrap timeout after %s — terminating instance, jobs reset to queued", elapsed.Truncate(time.Second)),
					DestroyProvider:   true,
					ResetJobs:         true,
					AttemptOutcome:    db.AttemptOutcomeOrphaned,
				}
			}
			remaining := termTimeout - elapsed
			return InstanceAction{
				Kind:         ActionDisplayOnly,
				StallMessage: fmt.Sprintf("bootstrap stalled — no activity (terminating in %s)", remaining.Truncate(time.Second)),
			}
		}
	}

	// 6. Stale heartbeat (display-only warning — the reconciler's probe logic is separate)
	if p.HeartbeatAge > heartbeatStaleThreshold && ci.Status == db.LaunchStatusRunning {
		return InstanceAction{
			Kind:         ActionDisplayOnly,
			StallMessage: "heartbeat stale",
		}
	}

	// 7. Failed self-destruct: all jobs finished but instance still running
	if ci.Status == db.LaunchStatusRunning && p.JobState.HasStartedJob && p.JobState.AllJobsTerminal && p.JobState.LatestJobEnd > 0 {
		if p.Now.Sub(time.Unix(p.JobState.LatestJobEnd, 0)) > 2*time.Minute {
			return InstanceAction{
				Kind:            ActionSelfDestructFailed,
				TerminalStatus:  db.LaunchStatusCompleted,
				StallMessage:    "all jobs finished but self-destruct failed — cleaning up",
				DestroyProvider: true,
			}
		}
	}

	// 8. Provider dead detection (with hysteresis)
	// Skip grace-period instances — a transient API failure shouldn't kill the session.
	if p.ProviderErr == nil && isProviderTerminal(p.ProviderInst) && !IsInstanceTerminal(ci.Status) && ci.Status != db.LaunchStatusGrace {
		return r.checkProviderDead(ci, p.ProviderInst, p.R2Client, p.Now)
	}

	return InstanceAction{Kind: ActionNone}
}

func (r *Reconciler) checkTerminationIntent(ci *db.Launch, inst *cloud.Instance, intent *instanceintent.Marker) InstanceAction {
	if intent == nil {
		return InstanceAction{Kind: ActionNone}
	}

	reason := intent.TerminationReason
	switch intent.TerminalStatus {
	case db.LaunchStatusCompleted:
		if reason == "" {
			reason = db.TerminationReasonCompleted
		}
		return InstanceAction{
			Kind:              ActionTerminationIntent,
			TerminalStatus:    db.LaunchStatusCompleted,
			TerminationReason: reason,
			DestroyProvider:   !isProviderTerminal(inst),
			AttemptOutcome:    db.AttemptOutcomeCompleted,
		}
	case db.LaunchStatusFailed:
		if reason == "" {
			reason = db.TerminationReasonJobFailure
		}
		return InstanceAction{
			Kind:              ActionTerminationIntent,
			TerminalStatus:    db.LaunchStatusFailed,
			TerminationReason: reason,
			DestroyProvider:   !isProviderTerminal(inst),
			ResetJobs:         true,
			AttemptOutcome:    db.AttemptOutcomeOrphaned,
		}
	default:
		return InstanceAction{Kind: ActionNone}
	}
}

func (r *Reconciler) checkProviderDead(ci *db.Launch, inst *cloud.Instance, r2Client *r2.Client, now time.Time) InstanceAction {
	// Check R2 for completion marker before assuming failure.
	if hasR2CompletionMarker(r2Client, ci.ID) {
		r.mu.Lock()
		delete(r.firstDeadAt, ci.ID)
		r.mu.Unlock()
		return InstanceAction{
			Kind:              ActionProviderDead,
			TerminalStatus:    db.LaunchStatusCompleted,
			TerminationReason: db.TerminationReasonCompleted,
			AttemptOutcome:    db.AttemptOutcomeCompleted,
		}
	}

	// Check R2 termination intent — the agent may have written it before dying.
	// This catches the race where the agent writes the intent, self-destructs,
	// and the provider shows "exited" before the reconciler's step 3 picks it up.
	if intent, err := fetchReconcileTerminationIntent(context.Background(), r2Client, ci.ID); err == nil && intent != nil {
		if action := r.checkTerminationIntent(ci, inst, intent); action.Kind != ActionNone {
			r.mu.Lock()
			delete(r.firstDeadAt, ci.ID)
			r.mu.Unlock()
			return action
		}
	}

	// Require the instance to appear dead for confirmTime before declaring terminal.
	confirmTime := r.confirmTime()
	r.mu.Lock()
	first, seen := r.firstDeadAt[ci.ID]
	if !seen {
		r.firstDeadAt[ci.ID] = now
		r.mu.Unlock()
		if confirmTime > 0 {
			return InstanceAction{Kind: ActionNone} // waiting to confirm
		}
	} else {
		r.mu.Unlock()
	}
	if confirmTime > 0 && now.Sub(first) < confirmTime {
		return InstanceAction{Kind: ActionNone} // still confirming
	}
	r.mu.Lock()
	delete(r.firstDeadAt, ci.ID)
	r.mu.Unlock()

	// Use "unknown" as the default reason — we can't determine what happened.
	// R2 markers may override below with a more specific reason.
	reason := db.TerminationReasonUnknown
	if ci.LaunchedAt != nil && inst != nil && inst.Status != "" {
		reason = inst.Status // echo provider status: "exited", "error", "destroyed", etc.
	}
	reason = failureTerminationReasonFromR2(context.Background(), r2Client, ci.ID, reason)

	return InstanceAction{
		Kind:              ActionProviderDead,
		TerminalStatus:    db.LaunchStatusFailed,
		TerminationReason: reason,
		ResetJobs:         true,
		AttemptOutcome:    db.AttemptOutcomeOrphaned,
	}
}

// ExecuteAction performs the side effects described by an InstanceAction: destroying
// the provider instance, updating DB status, and resetting/closing job attempts.
// Returns (reconciled, terminated).
func ExecuteAction(database *sql.DB, client cloud.Client, ci *db.Launch, action InstanceAction) (bool, bool) {
	if action.Kind == ActionNone || action.Kind == ActionDisplayOnly {
		return false, false
	}

	providerID := ci.EffectiveProviderID()

	if action.DestroyProvider && providerID != "" && client != nil {
		if err := client.DestroyInstance(providerID); err != nil {
			log.Printf("reconcile: failed to destroy instance %d (provider %s): %v", ci.ID, providerID, err)
		}
	}

	if action.TerminalStatus != "" {
		if err := db.UpdateLaunchStatus(database, ci.ID, action.TerminalStatus, action.TerminationReason); err != nil {
			log.Printf("reconcile: update instance %d status to %s: %v", ci.ID, action.TerminalStatus, err)
			return false, false
		}
		if eventKind := actionEventKind(action.Kind); eventKind != "" {
			_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
				EventKind: eventKind,
				LaunchID:  ci.ID,
				GPUSpec:   ci.GPUSpec,
				Detail:    action.StallMessage,
			})
		}
	}

	if action.ResetJobs {
		if resetCount, err := db.ResetLaunchJobs(database, ci.ID, action.AttemptOutcome); err != nil {
			log.Printf("reconcile: reset jobs for instance %d: %v", ci.ID, err)
		} else if resetCount > 0 {
			log.Printf("reconcile: reset %d jobs from instance %d to unplaced", resetCount, ci.ID)
		}
	} else if action.AttemptOutcome != "" && action.TerminalStatus == db.LaunchStatusCompleted {
		if err := db.CloseLaunchAttempts(database, ci.ID, action.AttemptOutcome); err != nil {
			log.Printf("reconcile: close attempts for instance %d: %v", ci.ID, err)
		}
	}

	return true, IsInstanceTerminal(action.TerminalStatus)
}

// actionEventKind maps an InstanceActionKind to a lifecycle event kind constant.
func actionEventKind(kind InstanceActionKind) string {
	switch kind {
	case ActionBootstrapStalled:
		return db.EventReconcileBootstrapTimeout
	case ActionGraceExpired:
		return db.EventReconcileGraceExpired
	case ActionProviderDead:
		return db.EventReconcileProviderDead
	case ActionSelfDestructFailed:
		return db.EventReconcileSelfDestructFail
	case ActionEmptyStatusTimeout:
		return db.EventReconcileEmptyStatus
	case ActionDonorComplete:
		return db.EventReconcileDonorComplete
	case ActionTerminationIntent:
		return db.EventReconcileTerminationIntent
	case ActionBootstrapComplete:
		return db.EventReconcileBootstrapComplete
	default:
		return ""
	}
}
