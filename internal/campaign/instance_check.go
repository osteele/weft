package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/oplog"
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
	ActionSelfDestructFailed                    // jobs terminal, instance lingering -> finalize launch + destroy
	ActionSetupStalled                          // setup phase unchanged too long -> fail
	ActionRunningStalled                        // running phase + stale heartbeat too long -> fail
	ActionPause                                 // provider stopped instance (preempted / account pause) -> launch=paused
	ActionResume                                // previously paused launch resumed by provider -> launch=running
)

// graceShutdownTimeout is how long after the grace deadline the coordinator
// waits for the agent to write a termination intent before force-destroying.
// The agent normally self-destructs within seconds of its deadline; this
// timeout is a safety net for unresponsive agents.
const graceShutdownTimeout = 2 * time.Minute

// InstanceAction describes what reconciliation action to take for a cloud instance.
type InstanceAction struct {
	Kind              InstanceActionKind
	TerminalStatus    string // db status to set (failed, completed, "")
	TerminationReason string // reason string for DB
	StallMessage      string // human-readable message
	DestroyProvider   bool   // whether to destroy the provider instance
	ResetJobs         bool   // whether to reset jobs to unplaced
	AttemptOutcome    string // attempt outcome when resetting/closing

	// Provider-state snapshot at the time the action was decided. Carried
	// through to the oplog at ExecuteAction time so post-mortems can see
	// the raw classifier inputs without re-fetching from the provider.
	ObservedProviderStatus         string
	ObservedProviderIntendedStatus string
	ObservedProviderStatusMsg      string
}

// JobState summarizes the aggregate state of jobs associated with a cloud instance.
type JobState struct {
	HasStartedJob    bool
	AllJobsTerminal  bool
	AllJobsCompleted bool
	AllJobsCanceled  bool
	AnyFailed        bool
	AnyOrphaned      bool
	LatestJobEnd     int64 // unix timestamp of latest job end, 0 if none
}

// ComputeJobState computes aggregate job state from a list of jobs.
func ComputeJobState(jobs []*db.Job, outcomes map[int64]string) JobState {
	s := JobState{
		AllJobsTerminal:  true,
		AllJobsCompleted: len(jobs) > 0,
		AllJobsCanceled:  len(jobs) > 0,
	}
	for _, j := range jobs {
		displayStatus := AttemptDisplayStatus(j, outcomes)
		if jobStartedOnInstance(j, displayStatus) {
			s.HasStartedJob = true
		}
		if !IsJobTerminal(displayStatus) {
			s.AllJobsTerminal = false
			s.AllJobsCompleted = false
			s.AllJobsCanceled = false
		} else {
			if displayStatus != db.StatusCompleted {
				s.AllJobsCompleted = false
			}
			if displayStatus != db.StatusCanceled {
				s.AllJobsCanceled = false
			}
			switch displayStatus {
			case db.StatusFailed, db.StatusDead, db.StatusKilled:
				s.AnyFailed = true
			case db.AttemptOutcomeOrphaned:
				s.AnyOrphaned = true
			}
		}
		if j.EndTime != nil && *j.EndTime > s.LatestJobEnd {
			s.LatestJobEnd = *j.EndTime
		}
	}
	return s
}

func jobStartedOnInstance(j *db.Job, displayStatus string) bool {
	if j == nil {
		return false
	}
	if j.StartTime > 0 {
		return true
	}
	if j.EndTime != nil && *j.EndTime > 0 {
		return true
	}
	switch displayStatus {
	case db.StatusQueued, db.StatusDraft:
		return false
	default:
		return true
	}
}

func (s JobState) TerminalLaunchStatus() (status string, reason string, ok bool) {
	if !s.HasStartedJob || !s.AllJobsTerminal {
		return "", "", false
	}
	switch {
	case s.AllJobsCompleted:
		return db.LaunchStatusCompleted, db.TerminationReasonCompleted, true
	case s.AllJobsCanceled:
		return db.LaunchStatusCancelled, db.TerminationReasonCancelled, true
	case s.AnyFailed:
		return db.LaunchStatusFailed, db.TerminationReasonJobFailure, true
	case s.AnyOrphaned:
		return db.LaunchStatusFailed, db.TerminationReasonInfraFailure, true
	default:
		return db.LaunchStatusFailed, db.TerminationReasonJobFailure, true
	}
}

// CheckInstanceParams holds all the pre-fetched state needed to evaluate an instance.
type CheckInstanceParams struct {
	CI             *db.Launch
	ProviderInst   *cloud.Instance
	ProviderErr    error
	R2Client       *r2.Client
	JobState       JobState
	PauseTolerant  bool
	InstancePhase  string // reconciled display/check phase
	BootstrapStage string // from R2
	HeartbeatAge   time.Duration
	Now            time.Time

	// TerminationIntent from R2 or DB (pre-fetched by caller)
	TerminationIntent *instanceintent.Marker

	// BootstrapSurvival holds adaptive bootstrap thresholds from historical
	// survival analysis. Nil means use package defaults.
	BootstrapSurvival *db.BootstrapSurvival

	// PhaseChangedAt is when the current InstancePhase was first observed.
	// Nil means unknown (phase stall check is skipped).
	PhaseChangedAt *time.Time

	// LastProviderStatusChangeAt is the time of the most recent
	// provider_status_transitions row for this launch. Used to anchor
	// rule 4b's pre-running timeout on time-since-status-went-non-running
	// instead of launched_at, so a previously-running instance gets a
	// fresh deadline when it transitions to a non-running status.
	// Nil means unknown — falls back to lifecycle start.
	LastProviderStatusChangeAt *time.Time

	// SetupSurvival holds adaptive setup-phase thresholds from historical
	// survival analysis. Nil means use package defaults.
	SetupSurvival *db.SetupSurvival
}

// CheckInstance evaluates what reconciliation action should be taken for a
// single cloud instance. This is the single source of truth for all instance
// reconciliation decisions. It does not mutate state, but may perform read-only
// R2 queries (donor ready check, completion marker).
//
// Hysteresis state (dead confirmation, probe failures) is tracked on the
// Reconciler. For WatchInstance, use a per-goroutine Reconciler.
func (r *Reconciler) CheckInstance(p CheckInstanceParams) (action InstanceAction) {
	ci := p.CI
	if ci == nil {
		return InstanceAction{Kind: ActionNone}
	}

	// Stamp the provider snapshot onto every non-no-op action so
	// ExecuteAction can record it in the oplog.
	defer func() {
		if action.Kind == ActionNone || action.Kind == ActionDisplayOnly || p.ProviderInst == nil {
			return
		}
		action.ObservedProviderStatus = p.ProviderInst.Status
		action.ObservedProviderIntendedStatus = p.ProviderInst.IntendedStatus
		action.ObservedProviderStatusMsg = p.ProviderInst.StatusMsg
	}()

	// 1. Grace expiry: deadline has passed.
	// The instance entered grace because a job failed. Don't reset jobs to
	// queued (ResetJobs: false) — the job already ran and errored, so
	// re-queuing would cause an infinite retry loop. Close the attempts as
	// "failed" instead.
	//
	// Instead of immediately destroying, we give the agent time to shut down
	// gracefully (upload logs, write termination intent). The coordinator
	// only force-destroys after graceShutdownTimeout with no agent response.
	if ci.Status == db.LaunchStatusGrace && ci.GraceDeadline != nil && p.Now.Unix() > *ci.GraceDeadline {
		if p.TerminationIntent != nil {
			// Agent already signaled intent to shut down — fall through
			// to step 3 which handles termination intent properly.
		} else {
			graceOverdueBy := p.Now.Sub(time.Unix(*ci.GraceDeadline, 0))
			if graceOverdueBy > graceShutdownTimeout {
				return InstanceAction{
					Kind:              ActionGraceExpired,
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: db.TerminationReasonJobFailure,
					StallMessage:      fmt.Sprintf("grace period expired %s ago with no agent shutdown — force terminating", graceOverdueBy.Truncate(time.Second)),
					DestroyProvider:   true,
					AttemptOutcome:    db.AttemptOutcomeFailed,
				}
			}
			return InstanceAction{
				Kind:         ActionDisplayOnly,
				StallMessage: "grace period expired — waiting for agent shutdown",
			}
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
		if action := r.checkTerminationIntent(ci, p.ProviderInst, p.TerminationIntent, p.PauseTolerant); action.Kind != ActionNone {
			return action
		}
	}

	// 4. Empty provider status timeout
	if p.ProviderInst != nil && p.ProviderInst.Status == "" {
		if lifecycleStart := cloudInstanceLifecycleStart(ci); lifecycleStart != nil {
			age := p.Now.Sub(*lifecycleStart)
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
	}

	// 4a. Provider status unavailable timeout: if polling repeatedly fails and
	// no job/phase progress is visible, fail closed instead of wedging forever.
	if p.ProviderInst == nil && p.ProviderErr != nil && (ci.Status == db.LaunchStatusLaunching || ci.Status == db.LaunchStatusRunning) &&
		!p.JobState.HasStartedJob && p.InstancePhase == "" && p.BootstrapStage != bootstrapStageReady {
		if lifecycleStart := cloudInstanceLifecycleStart(ci); lifecycleStart != nil {
			age := p.Now.Sub(*lifecycleStart)
			if age > maxPreRunningStatusTime {
				return InstanceAction{
					Kind:              ActionEmptyStatusTimeout,
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: db.TerminationReasonInfraFailure,
					StallMessage:      fmt.Sprintf("provider status unavailable for %s — terminating", age.Truncate(time.Second)),
					DestroyProvider:   true,
					ResetJobs:         true,
					AttemptOutcome:    db.AttemptOutcomeOrphaned,
				}
			}
		}
	}

	// 4a-resume. Previously paused launch is now running again at the provider.
	if ci.Status == db.LaunchStatusPaused && p.ProviderInst != nil &&
		p.ProviderInst.Status == cloud.ProviderStatusRunning {
		return InstanceAction{
			Kind:         ActionResume,
			StallMessage: "instance resumed by provider",
		}
	}

	// 4a-pause. Provider paused the instance (preemption or account-wide
	// pause). Vast.ai reports interruptible preemption as "offline";
	// Stopped covers explicit account/credit pauses. Reflect as paused in
	// the DB and wait up to stalePauseTimeout for resume; after that, give
	// up and fail. Runs regardless of preemptible flag so account-wide
	// credit pauses are also caught.
	if p.ProviderInst != nil && (p.ProviderInst.Status == cloud.ProviderStatusStopped || p.ProviderInst.Status == cloud.ProviderStatusOffline) {
		if lifecycleStart := cloudInstanceLifecycleStart(ci); lifecycleStart != nil {
			age := p.Now.Sub(*lifecycleStart)
			if age > stalePauseTimeout {
				reason := db.TerminationReasonPreempted
				if !p.PauseTolerant {
					reason = db.TerminationReasonProviderFailure
				}
				return InstanceAction{
					Kind:              ActionEmptyStatusTimeout,
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: reason,
					StallMessage:      fmt.Sprintf("instance %s for %s with no resume — giving up", p.ProviderInst.Status, age.Truncate(time.Minute)),
					DestroyProvider:   true,
					ResetJobs:         true,
					AttemptOutcome:    db.AttemptOutcomeOrphaned,
				}
			}
		}
		if ci.Status != db.LaunchStatusPaused {
			return InstanceAction{
				Kind:         ActionPause,
				StallMessage: fmt.Sprintf("provider reports %s instance — marking paused", p.ProviderInst.Status),
			}
		}
		return InstanceAction{
			Kind:         ActionDisplayOnly,
			StallMessage: fmt.Sprintf("provider reports %s instance; waiting for resume/replacement", p.ProviderInst.Status),
		}
	}

	// 4b. Stale non-running status. Anchored on the most recent provider
	// status transition (or lifecycle start when no transitions are
	// recorded), so a previously-running instance that drops back to a
	// non-running state gets a fresh deadline rather than inheriting an
	// already-exceeded one. Skip when IntendedStatus already signals
	// termination — step 8 catches that faster.
	if p.ProviderInst != nil && p.ProviderInst.Status != cloud.ProviderStatusRunning && p.ProviderInst.Status != "" &&
		!isProviderTerminalWithPolicy(p.ProviderInst, p.PauseTolerant) {
		if anchor := stalePreRunningAnchor(ci, p.LastProviderStatusChangeAt); anchor != nil {
			age := p.Now.Sub(*anchor)
			if age > maxPreRunningStatusTime {
				return InstanceAction{
					Kind:              ActionEmptyStatusTimeout,
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: db.TerminationReasonInfraFailure,
					StallMessage:      fmt.Sprintf("provider instance stuck in %q status for %s — terminating", p.ProviderInst.Status, age.Truncate(time.Second)),
					DestroyProvider:   true,
					ResetJobs:         true,
					AttemptOutcome:    db.AttemptOutcomeOrphaned,
				}
			}
		}
	}

	// 4c. Launching phase catch-all: no BootstrapOrigin means rule 5 and
	// spec BootstrapTimeout can't anchor. Fire on CreatedAt; reason
	// infra_failure so the retry path applies. See spec LaunchingPhaseTimeout.
	if ci.Status == db.LaunchStatusLaunching && ci.BootstrapOrigin() == nil {
		if start := cloudInstanceLifecycleStart(ci); start != nil && p.Now.Sub(*start) >= launchingPhaseTimeout {
			return InstanceAction{
				Kind:              ActionEmptyStatusTimeout,
				TerminalStatus:    db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonInfraFailure,
				StallMessage:      fmt.Sprintf("launching phase exceeded %s with no provider progress — terminating", launchingPhaseTimeout),
				DestroyProvider:   true,
				ResetJobs:         true,
				AttemptOutcome:    db.AttemptOutcomeOrphaned,
			}
		}
	}

	// 5. Bootstrap stall: no job progress after timeout. Fires during
	// `running` (post-bootstrap wait) and `launching` once BootstrapOrigin
	// is known. BootstrapOrigin prefers provider "running" time over
	// LaunchedAt so time spent in provider "loading" doesn't count.
	bootstrapPhaseActive := ci.Status == db.LaunchStatusRunning || ci.Status == db.LaunchStatusLaunching
	if bootstrapOrigin := ci.BootstrapOrigin(); bootstrapPhaseActive && bootstrapOrigin != nil && !p.JobState.HasStartedJob && p.InstancePhase == "" && p.BootstrapStage != bootstrapStageReady {
		elapsed := p.Now.Sub(time.Unix(*bootstrapOrigin, 0))

		warnTimeout := bootstrapWarnTimeout
		termTimeout := BootstrapTerminateTimeout
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

	// 5b. Setup phase stall: instance in setup/warmup phase for too long.
	// Running is DB-authoritative via phase reconciliation, so setup checks
	// only apply when reconciled phase is still setup/warmup.
	if ci.Status == db.LaunchStatusRunning {
		verb, _, _ := ParsePhaseJobID(p.InstancePhase)
		if verb == PhaseSetup || verb == PhaseGPUWarmup {
			phaseStart := p.PhaseChangedAt
			if phaseStart == nil {
				// Fallback for legacy rows where phase_changed_at was never recorded.
				phaseStart = cloudInstanceLifecycleStart(ci)
			}
			if phaseStart != nil {
				phaseAge := p.Now.Sub(*phaseStart)

				warnTimeout := defaultSetupStallWarn
				termTimeout := defaultSetupStallTerminate
				if p.SetupSurvival != nil {
					warnTimeout = p.SetupSurvival.WarnAfter
					termTimeout = p.SetupSurvival.TerminateAfter
				}

				if phaseAge >= termTimeout {
					return InstanceAction{
						Kind:              ActionSetupStalled,
						TerminalStatus:    db.LaunchStatusFailed,
						TerminationReason: db.TerminationReasonPhaseStall,
						StallMessage:      fmt.Sprintf("setup phase stalled for %s — terminating instance", phaseAge.Truncate(time.Second)),
						DestroyProvider:   true,
						ResetJobs:         true,
						AttemptOutcome:    db.AttemptOutcomeOrphaned,
					}
				}
				if phaseAge >= warnTimeout {
					remaining := termTimeout - phaseAge
					return InstanceAction{
						Kind:         ActionDisplayOnly,
						StallMessage: fmt.Sprintf("setup phase stalled for %s (terminating in %s)", phaseAge.Truncate(time.Second), remaining.Truncate(time.Second)),
					}
				}
			}
		}
	}

	// 5c. Running phase stall: heartbeat stale while in running phase.
	// A running job with a dead agent (stale heartbeat) should be terminated.
	// This does NOT check GPU utilization — jobs may legitimately not use the GPU.
	if ci.Status == db.LaunchStatusRunning && p.PhaseChangedAt != nil && p.HeartbeatAge > heartbeatStaleThreshold {
		verb, _, _ := ParsePhaseJobID(p.InstancePhase)
		if verb == PhaseRunning {
			phaseAge := p.Now.Sub(*p.PhaseChangedAt)
			if phaseAge >= runningStaleTerminate {
				return InstanceAction{
					Kind:              ActionRunningStalled,
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: db.TerminationReasonPhaseStall,
					StallMessage:      fmt.Sprintf("running phase stalled for %s with stale heartbeat — terminating instance", phaseAge.Truncate(time.Second)),
					DestroyProvider:   true,
					ResetJobs:         true,
					AttemptOutcome:    db.AttemptOutcomeOrphaned,
				}
			}
			if phaseAge >= runningStaleWarn {
				remaining := runningStaleTerminate - phaseAge
				return InstanceAction{
					Kind:         ActionDisplayOnly,
					StallMessage: fmt.Sprintf("running phase stalled for %s with stale heartbeat (terminating in %s)", phaseAge.Truncate(time.Second), remaining.Truncate(time.Second)),
				}
			}
		}
	}

	// 6. Failed self-destruct: jobs reached a terminal state but the instance is lingering.
	if ci.Status == db.LaunchStatusRunning && p.JobState.HasStartedJob && p.JobState.AllJobsTerminal {
		if terminalStatus, reason, ok := p.JobState.TerminalLaunchStatus(); ok {
			var sinceEnd time.Duration
			if p.JobState.LatestJobEnd > 0 {
				sinceEnd = p.Now.Sub(time.Unix(p.JobState.LatestJobEnd, 0))
			} else {
				// end_time unknown (was 0) — use a conservative fallback
				sinceEnd = 3 * time.Minute
			}
			if sinceEnd > 2*time.Minute {
				return InstanceAction{
					Kind:              ActionSelfDestructFailed,
					TerminalStatus:    terminalStatus,
					TerminationReason: reason,
					StallMessage:      "jobs reached terminal state but self-destruct failed — cleaning up",
					DestroyProvider:   true,
				}
			}
		}
	}

	// 7. Provider dead detection (with hysteresis)
	// Skip grace-period instances — a transient API failure shouldn't kill the session.
	if p.ProviderErr == nil && isProviderTerminalWithPolicy(p.ProviderInst, p.PauseTolerant) && !IsInstanceTerminal(ci.Status) && ci.Status != db.LaunchStatusGrace {
		var instDescr string
		if p.ProviderInst == nil {
			instDescr = "<nil>"
		} else {
			instDescr = fmt.Sprintf("status=%q intended=%q providerID=%q", p.ProviderInst.Status, p.ProviderInst.IntendedStatus, p.ProviderInst.ProviderID)
		}
		slog.Debug("reconcile: entering provider_dead path", "component", "reconcile", "instance", ci.ID, "inst", instDescr, "ci_status", ci.Status)
		return r.checkProviderDead(ci, p.ProviderInst, p.R2Client, p.JobState, p.Now)
	}

	// 8. Stale heartbeat (display-only warning — the reconciler's probe logic is separate)
	if p.HeartbeatAge > heartbeatStaleThreshold && ci.Status == db.LaunchStatusRunning {
		return InstanceAction{
			Kind:         ActionDisplayOnly,
			StallMessage: "heartbeat stale",
		}
	}

	return InstanceAction{Kind: ActionNone}
}

func (r *Reconciler) checkTerminationIntent(ci *db.Launch, inst *cloud.Instance, intent *instanceintent.Marker, pauseTolerant bool) InstanceAction {
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
			DestroyProvider:   !isProviderTerminalWithPolicy(inst, pauseTolerant),
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
			DestroyProvider:   !isProviderTerminalWithPolicy(inst, pauseTolerant),
			ResetJobs:         true,
			AttemptOutcome:    db.AttemptOutcomeOrphaned,
		}
	default:
		return InstanceAction{Kind: ActionNone}
	}
}

func (r *Reconciler) checkProviderDead(ci *db.Launch, inst *cloud.Instance, r2Client *r2.Client, jobState JobState, now time.Time) InstanceAction {
	// Check R2 for completion marker before assuming failure.
	if hasR2CompletionMarker(r2Client, ci.ID) {
		slog.Debug("provider dead: found R2 completion marker",
			"component", "reconcile", "instance", ci.ID)
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
		if action := r.checkTerminationIntent(ci, inst, intent, false); action.Kind != ActionNone {
			slog.Debug("provider dead: found termination intent",
				"component", "reconcile", "instance", ci.ID,
				"intent_status", intent.TerminalStatus, "intent_reason", intent.TerminationReason)
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

	if terminalStatus, reason, ok := jobState.TerminalLaunchStatus(); ok {
		return InstanceAction{
			Kind:              ActionProviderDead,
			TerminalStatus:    terminalStatus,
			TerminationReason: reason,
		}
	}

	// Use "unknown" as the default reason — we can't determine what happened.
	// R2 markers may override below with a more specific reason.
	reason := db.TerminationReasonUnknown
	if ci.LaunchedAt != nil && inst != nil && inst.Status != "" {
		reason = db.TerminationReasonProviderFailure
	}
	reason = failureTerminationReasonFromR2(context.Background(), r2Client, ci.ID, reason)

	slog.Warn("provider dead: no completion marker or termination intent found, orphaning jobs",
		"component", "reconcile", "instance", ci.ID, "reason", reason)
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

	// Pause/resume are non-terminal status flips; no destroy, no job reset.
	if action.Kind == ActionPause || action.Kind == ActionResume {
		newStatus := db.LaunchStatusPaused
		op := oplog.OpLaunchPaused
		if action.Kind == ActionResume {
			newStatus = db.LaunchStatusRunning
			op = oplog.OpLaunchResumed
		}
		if err := db.UpdateLaunchStatus(database, ci.ID, newStatus, "", action.StallMessage); err != nil {
			slog.Warn("failed to update instance status", "component", "reconcile", "instance", ci.ID, "status", newStatus, "error", err)
			return false, false
		}
		oplog.Log(op, oplog.WithDetail(formatActionDetail(ci.ID, action)))
		if eventKind := actionEventKind(action.Kind); eventKind != "" {
			_ = db.InsertLifecycleEvent(database, &db.LifecycleEvent{
				EventKind: eventKind,
				LaunchID:  ci.ID,
				GPUSpec:   ci.GPUSpec,
				Detail:    action.StallMessage,
			})
		}
		return true, false
	}

	providerID := ci.EffectiveProviderID()

	if action.DestroyProvider && providerID != "" && client != nil {
		if err := client.DestroyInstance(providerID); err != nil {
			slog.Warn("failed to destroy instance, deferring terminal status to next reconcile pass", "component", "reconcile", "instance", ci.ID, "provider", providerID, "error", err)
			return false, false
		}
	}

	if action.TerminalStatus != "" {
		if err := db.UpdateLaunchStatus(database, ci.ID, action.TerminalStatus, action.TerminationReason, action.StallMessage); err != nil {
			slog.Warn("failed to update instance status", "component", "reconcile", "instance", ci.ID, "status", action.TerminalStatus, "error", err)
			return false, false
		}
		op := oplog.OpLaunchTerminated
		if action.TerminalStatus == db.LaunchStatusFailed {
			op = oplog.OpLaunchLaunchFailed
		}
		oplog.Log(op, oplog.WithDetail(formatActionDetail(ci.ID, action)))
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
			slog.Warn("failed to reset jobs for instance", "component", "reconcile", "instance", ci.ID, "error", err)
		} else if resetCount > 0 {
			slog.Debug("reset jobs from instance to unplaced", "component", "reconcile", "count", resetCount, "instance", ci.ID)
		}
	} else if action.AttemptOutcome != "" {
		if err := db.CloseLaunchAttempts(database, ci.ID, action.AttemptOutcome); err != nil {
			slog.Warn("failed to close attempts for instance", "component", "reconcile", "instance", ci.ID, "error", err)
		}
	}

	return true, IsInstanceTerminal(action.TerminalStatus)
}

// actionKindName returns a stable string for an InstanceActionKind, used
// in oplog details. Falls back to the numeric form for unknown values.
func actionKindName(k InstanceActionKind) string {
	switch k {
	case ActionNone:
		return "none"
	case ActionDisplayOnly:
		return "display_only"
	case ActionGraceExpired:
		return "grace_expired"
	case ActionDonorComplete:
		return "donor_complete"
	case ActionTerminationIntent:
		return "termination_intent"
	case ActionEmptyStatusTimeout:
		return "empty_status_timeout"
	case ActionBootstrapStalled:
		return "bootstrap_stalled"
	case ActionBootstrapComplete:
		return "bootstrap_complete"
	case ActionSetupStalled:
		return "setup_stalled"
	case ActionRunningStalled:
		return "running_stalled"
	case ActionProviderDead:
		return "provider_dead"
	case ActionSelfDestructFailed:
		return "self_destruct_failed"
	case ActionPause:
		return "pause"
	case ActionResume:
		return "resume"
	default:
		return fmt.Sprintf("kind_%d", int(k))
	}
}

// formatActionDetail formats the per-action oplog detail string. Includes
// the provider-status snapshot captured by CheckInstance so post-mortems
// can identify status-classification bugs without re-fetching from the
// provider. Empty fields are omitted.
func formatActionDetail(launchID int64, action InstanceAction) string {
	parts := []string{fmt.Sprintf("launch_id=%d", launchID), fmt.Sprintf("action=%s", actionKindName(action.Kind))}
	if action.TerminationReason != "" {
		parts = append(parts, fmt.Sprintf("reason=%s", action.TerminationReason))
	}
	if action.ObservedProviderStatus != "" {
		parts = append(parts, fmt.Sprintf("status=%q", action.ObservedProviderStatus))
	}
	if action.ObservedProviderIntendedStatus != "" {
		parts = append(parts, fmt.Sprintf("intended=%q", action.ObservedProviderIntendedStatus))
	}
	if action.ObservedProviderStatusMsg != "" {
		parts = append(parts, fmt.Sprintf("status_msg=%q", action.ObservedProviderStatusMsg))
	}
	if action.StallMessage != "" {
		parts = append(parts, fmt.Sprintf("detail=%q", action.StallMessage))
	}
	return strings.Join(parts, " ")
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
	case ActionSetupStalled:
		return db.EventReconcileSetupStall
	case ActionRunningStalled:
		return db.EventReconcileRunningStall
	case ActionPause:
		return db.EventReconcileProviderPaused
	case ActionResume:
		return db.EventReconcileProviderResumed
	default:
		return ""
	}
}
