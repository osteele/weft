package campaign

import (
	"context"
	"database/sql"
	"errors"
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
	ActionNone                  InstanceActionKind = iota
	ActionDisplayOnly                              // stall message only, no state change
	ActionGraceExpired                             // grace deadline passed -> destroy + fail
	ActionDonorComplete                            // donor ready marker -> destroy + complete
	ActionTerminationIntent                        // R2 intent -> destroy + mark terminal
	ActionEmptyStatusTimeout                       // stuck with no provider status -> fail
	ActionBootstrapStalled                         // no bootstrap progress -> fail
	ActionBootstrapComplete                        // bootstrap stalled but R2 completion marker found -> complete
	ActionProviderDead                             // provider says dead (with hysteresis) -> fail/complete
	ActionSelfDestructFailed                       // jobs terminal, instance lingering -> finalize launch + destroy
	ActionSetupStalled                             // setup phase unchanged too long -> fail
	ActionRunningStalled                           // running phase + stale heartbeat too long -> fail
	ActionIdleAfterReadyTimeout                    // agent reached ready but never started a job -> fail
	ActionPause                                    // provider stopped instance (preempted / account pause) -> launch=paused
	ActionResume                                   // previously paused launch resumed by provider -> launch=running
	ActionHedgeCull                                // hedge cohort sibling reached ready first -> destroy this loser
	ActionTerminalLivePhase                        // live phase reports running job that is already terminal in DB
	ActionStaleHeartbeat                           // heartbeat stale + agent probe dead/unreachable -> fail (reconciler-only)
)

// graceShutdownTimeout is how long after the grace deadline Weft
// waits for the agent to write a termination intent before force-destroying.
// The agent normally self-destructs within seconds of its deadline; this
// timeout is a safety net for unresponsive agents.
const graceShutdownTimeout = 2 * time.Minute

// InstanceAction describes what reconciliation action to take for a cloud instance.
type InstanceAction struct {
	Kind              InstanceActionKind
	TerminalStatus    string    // db status to set (failed, completed, "")
	TerminalAt        time.Time // authoritative terminal time; zero means execution time
	TerminationReason string    // reason string for DB
	StallMessage      string    // human-readable message
	DestroyProvider   bool      // whether to destroy the provider instance
	ResetJobs         bool      // whether to reset jobs to unplaced
	AttemptOutcome    string    // attempt outcome when resetting/closing

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
		if jobStartedOnInstance(j) {
			s.HasStartedJob = true
		}
		// A superseded attempt migrated to a newer attempt (e.g. requeued by
		// `weft edit --retry`); its outcome belongs to that new attempt, not to
		// this launch. Treat it as terminal-and-neutral: it neither blocks
		// AllJobsTerminal nor drags the launch to a failed/canceled terminal
		// status. Without this, a launch whose only job was requeued away sits
		// non-terminal forever and is never reaped.
		if displayStatus != db.AttemptOutcomeSuperseded {
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
		}
		if j.EndTime != nil && *j.EndTime > s.LatestJobEnd {
			s.LatestJobEnd = *j.EndTime
		}
	}
	return s
}

// jobStartedOnInstance reports whether the job actually executed.
// start_time is the only reliable signal; see specs/campaign-lifecycle.allium
// Instance.has_started_job for the watchdog rationale and accepted edge case.
func jobStartedOnInstance(j *db.Job) bool {
	if j == nil {
		return false
	}
	return j.StartTime > 0
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
	CI           *db.Launch
	ProviderInst *cloud.Instance
	ProviderErr  error
	// ProviderStatusUnknownFor is how long provider status polling has been
	// continuously failing for this launch — the unbroken stretch of passes
	// where ProviderInst was nil with a non-nil ProviderErr, tracked by
	// Reconciler.noteProviderStatusPoll. Zero on the first failing pass and
	// whenever the most recent poll returned instance data. Only meaningful
	// when ProviderInst == nil && ProviderErr != nil; consulted by the
	// watchdog rules that defer terminating interruptible launches while
	// provider status is unknown (a failed poll is not evidence of death —
	// an outbid interruptible instance must be waited on, not destroyed).
	ProviderStatusUnknownFor time.Duration
	R2Client                 *r2.Client
	JobState                 JobState
	PauseTolerant            bool
	InstancePhase            string // reconciled display/check phase
	BootstrapStage           string // from R2
	// OnStartStage is the last value of the OnStart shell's stage marker
	// (e.g. apt-installing, rclone-failed, onstart-deps-ready) — the step
	// the provider's OnStart script reached before bootstrap.sh took over.
	// Empty once bootstrap.sh starts (BootstrapStage takes over) or once a
	// job phase is reported. OnStartStageChangedAt is the R2 last-modified
	// time of that marker, i.e. when the stage last advanced; a frozen value
	// means the OnStart chain stalled mid-install. Used by rules 4e/4f.
	OnStartStage          string
	OnStartStageChangedAt *time.Time
	HeartbeatAge          time.Duration
	Heartbeat             *HeartbeatSample
	HeartbeatUnknown      bool
	InstancePhaseUnknown  bool
	BootstrapStageUnknown bool
	Now                   time.Time

	// TerminationIntent from R2 or DB (pre-fetched by caller)
	TerminationIntent *instanceintent.Marker

	// BootstrapSurvival holds bootstrap thresholds from historical survival
	// analysis. Individual thresholds may remain configured defaults when the
	// observed curve does not cross their cutoff. Nil uses package defaults.
	BootstrapSurvival *db.BootstrapSurvival

	// PhaseChangedAt is when the current InstancePhase was first observed.
	// Nil means unknown (phase stall check is skipped).
	PhaseChangedAt *time.Time

	// Structured job progress is a semantic task-progress signal emitted by
	// the job. ChangedAt advances only when the (job, phase, percent) tuple
	// changes; repeated reports of the same value do not prove progress.
	JobProgressID        int64
	JobProgressPct       int
	JobProgressChangedAt *time.Time

	// LastProviderStatusChangeAt is the time of the most recent
	// provider_status_transitions row for this launch. Used to anchor
	// rule 4b's pre-running timeout on time-since-status-went-non-running
	// instead of launched_at, so a previously-running instance gets a
	// fresh deadline when it transitions to a non-running status.
	// Nil means unknown — falls back to lifecycle start.
	LastProviderStatusChangeAt *time.Time

	// SetupSurvival holds setup-phase thresholds from historical survival
	// analysis. Individual thresholds may remain configured defaults when the
	// observed curve does not cross their cutoff. Nil uses package defaults.
	SetupSurvival *db.SetupSurvival

	// HedgeCohortHasReadySibling reports whether another launch in this
	// launch's hedge cohort has reached agent_ready. Pre-computed by the
	// caller because CheckInstance must stay pure (no DB handle).
	HedgeCohortHasReadySibling bool

	// OnStartProbePresent is true when the OnStart script's first-line
	// probe (a curl PUT to the presigned URL) has landed in R2. The
	// probe is the earliest evidence the container actually executed
	// OnStart with outbound network. Used by the dud-provider watchdog
	// (rule 4d) to distinguish "provider says running but container never
	// started" from "container running, OnStart in flight".
	OnStartProbePresent bool

	// BootstrapActivitySeen is true when current or historical bootstrap
	// stage data exists for the launch. It protects the dud-provider
	// watchdog from false-firing on transient empty R2 reads after
	// bootstrap has already shown progress.
	BootstrapActivitySeen bool

	// RunningPhaseJobTerminalSince is set when InstancePhase reports
	// running:<job> but that job's attempt on this launch is already
	// terminal in the DB. A fresh heartbeat can otherwise keep a dead
	// launch alive forever while queued attempts stay attached to it.
	RunningPhaseJobID            int64
	RunningPhaseJobStatus        string
	RunningPhaseJobTerminalSince *time.Time
}

// effectiveMaxEmptyStatusTime returns the empty-status kill threshold.
// A learned bootstrap terminate threshold replaces the legacy 1-minute
// constant so slow provider status propagation is not misclassified as a
// provider failure.
func (p CheckInstanceParams) effectiveMaxEmptyStatusTime() time.Duration {
	if d, ok := p.BootstrapSurvival.LearnedTerminate(); ok {
		return d
	}
	return maxEmptyStatusTime
}

// effectiveMaxPreRunningStatusTime returns the pre-running-status kill
// threshold. Uses the learned bootstrap survival terminate threshold when one
// exists, otherwise the legacy constant.
func (p CheckInstanceParams) effectiveMaxPreRunningStatusTime() time.Duration {
	if d, ok := p.BootstrapSurvival.LearnedTerminate(); ok {
		return d
	}
	return maxPreRunningStatusTime
}

// effectiveLaunchingPhaseTimeout returns the launching-phase safety-net
// timeout. Uses the learned bootstrap survival terminate threshold when one
// exists.
func (p CheckInstanceParams) effectiveLaunchingPhaseTimeout() time.Duration {
	if d, ok := p.BootstrapSurvival.LearnedTerminate(); ok {
		return d
	}
	return launchingPhaseTimeout
}

// effectiveDudVastTimeout returns the dud-provider detection window.
// Uses the learned bootstrap survival terminate threshold when one exists.
func (p CheckInstanceParams) effectiveDudVastTimeout() time.Duration {
	if d, ok := p.BootstrapSurvival.LearnedTerminate(); ok {
		return d
	}
	return dudVastTimeout
}

// effectiveOnStartStallTimeout returns the per-stage OnStart stall timeout.
// Uses the learned setup survival terminate threshold when one exists.
func (p CheckInstanceParams) effectiveOnStartStallTimeout() time.Duration {
	if d, ok := p.SetupSurvival.LearnedTerminate(); ok {
		return d
	}
	return onStartStallTimeout
}

// effectiveOnStartTotalActiveTimeout returns the total OnStart active-time
// cap. Uses the learned setup survival terminate threshold when one exists.
func (p CheckInstanceParams) effectiveOnStartTotalActiveTimeout() time.Duration {
	if d, ok := p.SetupSurvival.LearnedTerminate(); ok {
		return d
	}
	return onStartTotalActiveTimeout
}

// deferTerminationForUnknownStatus returns a display-only deferral action
// when a watchdog termination must be held back: the launch is interruptible,
// provider status is unknown (the most recent poll failed — ProviderInst nil
// with a non-nil ProviderErr), and status has been unknown for less than
// stalePauseTimeout. An outbid interruptible instance stops heartbeating and
// making phase progress while it waits for resume or a bid raise; when
// status polling is also failing (e.g. `vastai show instances` timing out on
// a slow network) the watchdogs cannot distinguish "outbid, recoverable"
// from "dead", so they defer instead of destroying a recoverable rental.
// Past stalePauseTimeout — the same bound the pause-wait path uses — the
// caller terminates as usual. Non-interruptible launches and passes where
// the poll succeeded are never deferred. context describes the watchdog
// condition that would have fired (e.g. "agent heartbeat stale for 5m").
func deferTerminationForUnknownStatus(p CheckInstanceParams, context string) (InstanceAction, bool) {
	if p.CI == nil || p.CI.InstanceType != cloud.InstanceTypeInterruptible {
		return InstanceAction{}, false
	}
	if p.ProviderInst != nil || p.ProviderErr == nil {
		return InstanceAction{}, false // provider status known (or never polled)
	}
	if errors.Is(p.ProviderErr, cloud.ErrInstanceNotFound) {
		// A definitive not-found is an answer, not a failed poll: the provider
		// says the instance no longer exists, so deferring only delays requeue
		// of its jobs. The dead-confirm hysteresis still absorbs single blips.
		return InstanceAction{}, false
	}
	if p.ProviderStatusUnknownFor >= stalePauseTimeout {
		return InstanceAction{}, false
	}
	return InstanceAction{
		Kind:         ActionDisplayOnly,
		StallMessage: fmt.Sprintf("%s but provider status unknown (poll failing for %s) — deferring termination for interruptible instance", context, p.ProviderStatusUnknownFor.Truncate(time.Second)),
	}, true
}

// CheckInstance evaluates what reconciliation action should be taken for a
// single cloud instance. This is the single source of truth for all instance
// reconciliation decisions. It does not mutate state, but may perform read-only
// R2 queries (donor ready check, completion marker).
//
// Hysteresis state (dead confirmation, probe failures) is tracked on the
// Reconciler. For WatchInstance, use a per-goroutine Reconciler.
func (r *Reconciler) CheckInstance(p CheckInstanceParams) InstanceAction {
	action := r.checkInstance(p)
	if !isDestructiveLivenessAction(action.Kind) {
		return action
	}
	context := action.StallMessage
	if context == "" {
		context = "liveness watchdog would terminate instance"
	}
	if deferred, ok := deferTerminationForUnknownStatus(p, context); ok {
		return deferred
	}
	return action
}

func isDestructiveLivenessAction(kind InstanceActionKind) bool {
	switch kind {
	case ActionEmptyStatusTimeout, ActionBootstrapStalled, ActionSetupStalled,
		ActionRunningStalled, ActionIdleAfterReadyTimeout, ActionTerminalLivePhase,
		ActionStaleHeartbeat:
		return true
	default:
		return false
	}
}

func (r *Reconciler) checkInstance(p CheckInstanceParams) (action InstanceAction) {
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
	// gracefully (upload logs, write termination intent). Weft
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

	// 1a. Hedge-cohort cull: a non-survivor probe is terminated once a
	// sibling reaches agent_ready. Gate on AgentReadyAtUnix == nil so
	// the survivor itself is never the one culled. ResetJobs orphans
	// any jobs the loser was holding so the next reuse pass migrates
	// them onto the surviving probe.
	// See campaign-lifecycle.allium § HedgeCohortCull.
	if ci.HedgeCohortID != nil && ci.AgentReadyAtUnix == nil &&
		(ci.Status == db.LaunchStatusLaunching || ci.Status == db.LaunchStatusRunning) &&
		p.HedgeCohortHasReadySibling {
		return InstanceAction{
			Kind:              ActionHedgeCull,
			TerminalStatus:    db.LaunchStatusCancelled,
			TerminationReason: db.TerminationReasonCancelled,
			StallMessage:      "hedge cohort sibling reached ready first — culling",
			DestroyProvider:   true,
			ResetJobs:         true,
			AttemptOutcome:    db.AttemptOutcomeCancelled,
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

	// 3a. The heartbeat sidecar is alive, but it reports that the campaign
	// agent process has exited. Treat this as a genuine agent death instead of
	// waiting for the heartbeat object to go stale.
	if ci.Status == db.LaunchStatusRunning && p.Heartbeat != nil && p.Heartbeat.AgentAlive != nil && !*p.Heartbeat.AgentAlive {
		stallMessage := "campaign agent exited while instance was still running — terminating instance"
		if p.Heartbeat.AgentFatal != "" {
			stallMessage = "campaign agent panicked: " + p.Heartbeat.AgentFatal + " — terminating instance"
		}
		return InstanceAction{
			Kind:              ActionRunningStalled,
			TerminalStatus:    db.LaunchStatusFailed,
			TerminationReason: db.TerminationReasonInfraFailure,
			StallMessage:      stallMessage,
			DestroyProvider:   true,
			ResetJobs:         true,
			AttemptOutcome:    db.AttemptOutcomeOrphaned,
		}
	}

	// 3b. Impossible live phase: the sidecar/phase marker is fresh and claims
	// a job is running, but the DB already has that launch attempt in a
	// terminal state. Treat this as an unhealthy agent/phase loop and orphan
	// the remaining queued attempts so they can be placed elsewhere.
	if ci.Status == db.LaunchStatusRunning && p.RunningPhaseJobTerminalSince != nil {
		age := p.Now.Sub(*p.RunningPhaseJobTerminalSince)
		if age > 2*time.Minute {
			status := strings.TrimSpace(p.RunningPhaseJobStatus)
			if status == "" {
				status = "terminal"
			}
			reason := db.TerminationReasonInfraFailure
			switch status {
			case db.StatusFailed, db.StatusDead, db.StatusKilled:
				reason = db.TerminationReasonJobFailure
			}
			return InstanceAction{
				Kind:              ActionTerminalLivePhase,
				TerminalStatus:    db.LaunchStatusFailed,
				TerminationReason: reason,
				StallMessage:      fmt.Sprintf("live phase reports wj%d running, but its attempt is %s for %s — terminating instance", p.RunningPhaseJobID, status, age.Truncate(time.Second)),
				DestroyProvider:   true,
				ResetJobs:         true,
				AttemptOutcome:    db.AttemptOutcomeOrphaned,
			}
		}
	}

	// 4. Empty provider status timeout
	if p.ProviderInst != nil && p.ProviderInst.Status == "" {
		if lifecycleStart := cloudInstanceLifecycleStart(ci); lifecycleStart != nil {
			age := p.Now.Sub(*lifecycleStart)
			if age > p.effectiveMaxEmptyStatusTime() {
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

	// 4a-resume. Previously paused launch is now running again at the provider.
	if ci.Status == db.LaunchStatusPaused && p.ProviderInst != nil &&
		p.ProviderInst.Status == cloud.ProviderStatusRunning {
		return InstanceAction{
			Kind:         ActionResume,
			StallMessage: "instance resumed by provider",
		}
	}

	// 4a-pause. Provider paused the instance. Vast.ai reports interruptible
	// preemption as "offline"; stopped covers explicit provider pauses such as
	// account/credit holds. Only pause statuses that can reasonably resume are
	// reflected as LaunchStatusPaused.
	if p.ProviderInst != nil && isRecoverablePausedProviderStatus(p.ProviderInst.Status, p.PauseTolerant) {
		if pauseStart := stalePreRunningAnchor(ci, p.LastProviderStatusChangeAt); pauseStart != nil {
			age := p.Now.Sub(*pauseStart)
			if age > stalePauseTimeout {
				reason := db.TerminationReasonPreempted
				outcome := db.AttemptOutcomePreempted
				if !p.PauseTolerant {
					reason = db.TerminationReasonProviderFailure
					outcome = db.AttemptOutcomeOrphaned
				}
				return InstanceAction{
					Kind:              ActionEmptyStatusTimeout,
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: reason,
					StallMessage:      fmt.Sprintf("instance %s for %s with no resume — giving up", p.ProviderInst.Status, age.Truncate(time.Minute)),
					DestroyProvider:   true,
					ResetJobs:         true,
					AttemptOutcome:    outcome,
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
			if age > p.effectiveMaxPreRunningStatusTime() {
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
		timeout := p.effectiveLaunchingPhaseTimeout()
		if start := cloudInstanceLifecycleStart(ci); start != nil && p.Now.Sub(*start) >= timeout {
			if action, ok := deferTerminationForUnknownStatus(p, fmt.Sprintf("launching phase exceeded %s", timeout)); ok {
				return action
			}
			return InstanceAction{
				Kind:              ActionEmptyStatusTimeout,
				TerminalStatus:    db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonInfraFailure,
				StallMessage:      fmt.Sprintf("launching phase exceeded %s with no provider progress — terminating", timeout),
				DestroyProvider:   true,
				ResetJobs:         true,
				AttemptOutcome:    db.AttemptOutcomeOrphaned,
			}
		}
	}

	// 4d. Dud-provider detection. The provider reports the rental as `running`
	// (LaunchedAt is set), but no agent activity has appeared in R2
	// after the dud timeout: no OnStart probe, no heartbeat, no phase,
	// no current or historical bootstrap stage. This is a host-level binary failure
	// signature (zombie offer, wedged docker daemon, image-pull block)
	// and would otherwise wait the full adaptive bootstrap deadline
	// (often 90+ min). The full conjunction is intentional: any single
	// signal of life means a downstream rule (heartbeat-stale,
	// bootstrap-stalled) is the right adjudicator.
	// See campaign-lifecycle.allium § DudVastDetection.
	if ci.Status == db.LaunchStatusRunning &&
		ci.LaunchedAt != nil && *ci.LaunchedAt > 0 &&
		ci.AgentReadyAtUnix == nil &&
		!p.OnStartProbePresent &&
		!p.BootstrapActivitySeen &&
		!p.HeartbeatUnknown &&
		!p.InstancePhaseUnknown &&
		!p.BootstrapStageUnknown &&
		p.Heartbeat == nil &&
		p.HeartbeatAge == 0 &&
		p.InstancePhase == "" &&
		p.BootstrapStage == "" {
		runningAt := time.Unix(*ci.LaunchedAt, 0)
		if p.Now.Sub(runningAt) >= p.effectiveDudVastTimeout() {
			return InstanceAction{
				Kind:              ActionEmptyStatusTimeout,
				TerminalStatus:    db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonInfraFailure,
				StallMessage:      fmt.Sprintf("dud provider: %s post-running with no agent activity — terminating instance, jobs reset to queued", p.Now.Sub(runningAt).Truncate(time.Second)),
				DestroyProvider:   true,
				ResetJobs:         true,
				AttemptOutcome:    db.AttemptOutcomeOrphaned,
			}
		}
	}

	// 4e/4f. OnStart chain failed or stalled. The OnStart probe landed (so
	// rule 4d deferred to "a downstream adjudicator"), but the OnStart shell
	// then failed or hung during the apt/uv/rclone install, before bootstrap.sh
	// ran — so neither the bootstrap `failed:` marker (rule 5a) nor the
	// bootstrap deadline's progress signal ever appears, and the instance would
	// otherwise wait the full adaptive deadline (often 1h+). These two rules
	// adjudicate that window using the OnStart stage marker the watchdog now
	// carries. Gated pre-bootstrap (BootstrapStage empty), pre-ready, pre-job.
	// See campaign-lifecycle.allium § OnStartChainWatchdog.
	onStartActive := (ci.Status == db.LaunchStatusRunning || ci.Status == db.LaunchStatusLaunching) &&
		ci.AgentReadyAtUnix == nil &&
		!p.JobState.HasStartedJob &&
		p.InstancePhase == "" &&
		p.BootstrapStage == "" &&
		p.OnStartStage != ""
	if onStartActive {
		// 4e. A definitive install failure marker — terminate now.
		if isOnStartFailureStage(p.OnStartStage) {
			return InstanceAction{
				Kind:              ActionBootstrapStalled,
				TerminalStatus:    db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonInfraFailure,
				StallMessage:      fmt.Sprintf("OnStart failed at %s — terminating instance, jobs reset to queued", p.OnStartStage),
				DestroyProvider:   true,
				ResetJobs:         true,
				AttemptOutcome:    db.AttemptOutcomeOrphaned,
			}
		}
		// 4f. An in-progress stage frozen past the stall timeout — the chain
		// hung (e.g. an unresponsive apt mirror) and will not recover.
		if p.OnStartStageChangedAt != nil {
			stalled := p.Now.Sub(*p.OnStartStageChangedAt)
			if stalled >= p.effectiveOnStartStallTimeout() {
				return InstanceAction{
					Kind:              ActionBootstrapStalled,
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: db.TerminationReasonInfraFailure,
					StallMessage:      fmt.Sprintf("OnStart stalled at %s for %s — terminating instance, jobs reset to queued", p.OnStartStage, stalled.Truncate(time.Second)),
					DestroyProvider:   true,
					ResetJobs:         true,
					AttemptOutcome:    db.AttemptOutcomeOrphaned,
				}
			}
		}
		// 4g. Total OnStart time exceeded without ever reaching bootstrap.sh
		// or agent-ready. Rule 4f measures the stall from the marker's R2
		// last-modified, so a provider that re-runs a failed OnStart from the
		// top (e.g. RunPod) refreshes the marker every loop and 4f never
		// fires. Anchoring on FirstOnStartProbeSeenUnix (set-once, immune to
		// marker rewrites) reaps the looping instance instead of letting it
		// bleed to the ~2h adaptive bootstrap deadline.
		if ci.FirstOnStartProbeSeenUnix != nil {
			active := p.Now.Sub(time.Unix(*ci.FirstOnStartProbeSeenUnix, 0))
			if active >= p.effectiveOnStartTotalActiveTimeout() {
				return InstanceAction{
					Kind:              ActionBootstrapStalled,
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: db.TerminationReasonInfraFailure,
					StallMessage:      fmt.Sprintf("OnStart active %s without reaching bootstrap (marker at %s; likely restarting/looping) — terminating instance, jobs reset to queued", active.Truncate(time.Second), p.OnStartStage),
					DestroyProvider:   true,
					ResetJobs:         true,
					AttemptOutcome:    db.AttemptOutcomeOrphaned,
				}
			}
		}
	}

	// 5. Bootstrap stall: no job progress past the per-launch deadline.
	// Fires during `running` (post-bootstrap wait) and `launching`. The
	// deadline is set once at LaunchInstance time and backfilled for
	// pre-#4 rows (specs/job-move.allium § InstanceReadiness); the
	// reconciler also honors later adaptive survival thresholds so already
	// running launches are not killed by an older, shorter stamped deadline.
	bootstrapPhaseActive := ci.Status == db.LaunchStatusRunning || ci.Status == db.LaunchStatusLaunching
	if bootstrapPhaseActive && strings.HasPrefix(strings.TrimSpace(p.BootstrapStage), "failed:") && !p.JobState.HasStartedJob {
		return InstanceAction{
			Kind:              ActionBootstrapStalled,
			TerminalStatus:    db.LaunchStatusFailed,
			TerminationReason: db.TerminationReasonInfraFailure,
			StallMessage:      fmt.Sprintf("bootstrap failed at %s — terminating instance, jobs reset to queued", BootstrapStageLabel(p.BootstrapStage)),
			DestroyProvider:   true,
			ResetJobs:         true,
			AttemptOutcome:    db.AttemptOutcomeOrphaned,
		}
	}
	if bootstrapPhaseActive && ci.BootstrapDeadlineUnix != nil && !p.JobState.HasStartedJob && p.InstancePhase == "" && p.BootstrapStage != bootstrapStageReady {
		deadline := time.Unix(*ci.BootstrapDeadlineUnix, 0)
		warnTimeout := bootstrapWarnTimeout
		termTimeout := BootstrapTerminateTimeout
		if p.BootstrapSurvival != nil {
			// bootstrapWarnTimeout/BootstrapTerminateTimeout equal the survival
			// defaults, so reading only learned values leaves this unchanged
			// while keeping defaults out of the kill decision.
			if learned, ok := p.BootstrapSurvival.LearnedWarn(); ok {
				warnTimeout = learned
			}
			if learned, ok := p.BootstrapSurvival.LearnedTerminate(); ok {
				termTimeout = learned
			}
			if originUnix := ci.BootstrapOrigin(); originUnix != nil {
				learnedDeadline := time.Unix(*originUnix, 0).Add(termTimeout)
				if learnedDeadline.After(deadline) {
					deadline = learnedDeadline
				}
			}
		}
		remaining := deadline.Sub(p.Now)
		warnRemaining := termTimeout - warnTimeout
		if remaining <= warnRemaining {
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
			stageLabel := strings.TrimSpace(BootstrapStageLabel(p.BootstrapStage))
			if stageLabel == "" || p.BootstrapStage == bootstrapStageReady {
				stageLabel = "bootstrap"
			}
			if remaining <= 0 {
				elapsed := termTimeout - remaining
				if action, ok := deferTerminationForUnknownStatus(p, fmt.Sprintf("%s timeout after %s", stageLabel, elapsed.Truncate(time.Second))); ok {
					return action
				}
				return InstanceAction{
					Kind:              ActionBootstrapStalled,
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: db.TerminationReasonBootstrapTimeout,
					StallMessage:      fmt.Sprintf("%s timeout after %s — terminating instance, jobs reset to queued", stageLabel, elapsed.Truncate(time.Second)),
					DestroyProvider:   true,
					ResetJobs:         true,
					AttemptOutcome:    db.AttemptOutcomeOrphaned,
				}
			}
			return InstanceAction{
				Kind:         ActionDisplayOnly,
				StallMessage: fmt.Sprintf("%s stalled — no activity (terminating in %s)", stageLabel, remaining.Truncate(time.Second)),
			}
		}
	}

	// 5a. Idle after agent_ready: ready but no job ever started — see
	// idleAfterReadyTimeout doc for the watchdog dead-zone this closes.
	if ci.Status == db.LaunchStatusRunning && ci.AgentReadyAtUnix != nil &&
		!p.JobState.HasStartedJob && p.InstancePhase == "" {
		readyAge := p.Now.Sub(time.Unix(*ci.AgentReadyAtUnix, 0))
		if readyAge >= idleAfterReadyTimeout {
			return InstanceAction{
				Kind:              ActionIdleAfterReadyTimeout,
				TerminalStatus:    db.LaunchStatusFailed,
				TerminationReason: db.TerminationReasonInfraFailure,
				StallMessage:      fmt.Sprintf("agent reached ready %s ago but never started a job — terminating instance, jobs reset to queued", readyAge.Truncate(time.Second)),
				DestroyProvider:   true,
				ResetJobs:         true,
				AttemptOutcome:    db.AttemptOutcomeOrphaned,
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
			// Launch age is not setup age. Legacy rows may lack a phase timestamp;
			// that absence is unknown timing evidence and cannot justify a
			// destructive setup-stall decision.
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

	// 5c. Running task stall: structured progress has not changed. Agent
	// heartbeat is deliberately irrelevant here: it proves the control plane is
	// alive, not that the user's task is advancing. Uninstrumented jobs do not
	// enter this watchdog; their explicit runtime and spend limits remain the
	// bound.
	if ci.Status == db.LaunchStatusRunning && p.JobProgressChangedAt != nil {
		verb, phaseJobID, ok := ParsePhaseJobID(p.InstancePhase)
		if ok && verb == PhaseRunning && phaseJobID == p.JobProgressID && p.JobProgressPct >= 0 {
			progressAge := p.Now.Sub(*p.JobProgressChangedAt)
			if progressAge >= runningStaleTerminate {
				if action, ok := deferTerminationForUnknownStatus(p, fmt.Sprintf("structured job progress unchanged for %s", progressAge.Truncate(time.Second))); ok {
					return action
				}
				return InstanceAction{
					Kind:              ActionRunningStalled,
					TerminalStatus:    db.LaunchStatusFailed,
					TerminationReason: db.TerminationReasonPhaseStall,
					StallMessage:      fmt.Sprintf("structured job progress unchanged for %s — terminating instance", progressAge.Truncate(time.Second)),
					DestroyProvider:   true,
					ResetJobs:         true,
					AttemptOutcome:    db.AttemptOutcomeOrphaned,
				}
			}
			if progressAge >= runningStaleWarn {
				remaining := runningStaleTerminate - progressAge
				return InstanceAction{
					Kind:         ActionDisplayOnly,
					StallMessage: fmt.Sprintf("structured job progress unchanged for %s (terminating in %s)", progressAge.Truncate(time.Second), remaining.Truncate(time.Second)),
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

	// 7. Provider dead detection (with hysteresis). Requires a positively
	// observed terminal provider status — a non-nil instance reporting
	// exited/destroyed/etc. A nil instance never qualifies: nil with a nil
	// error means the launch was not polled this pass (create still in
	// flight, or no client configured for its provider); nil with
	// ErrInstanceNotFound is non-authoritative because vast.ai transiently
	// reports not-found for still-booting instances, so confirmed-absence
	// reaping is left to the heartbeat/bootstrap watchdogs above (which
	// not-found does not defer — see deferTerminationForUnknownStatus);
	// nil with any other error is a failed poll. See
	// providerConfirmedTerminal for the confirmation-path counterpart.
	// Skip grace-period instances — a transient API failure shouldn't kill the session.
	providerObserved := p.ProviderErr == nil && p.ProviderInst != nil
	providerTerminal := providerObserved && isProviderTerminalWithPolicy(p.ProviderInst, p.PauseTolerant)
	if providerTerminal && !IsInstanceTerminal(ci.Status) && ci.Status != db.LaunchStatusGrace {
		instDescr := fmt.Sprintf("status=%q intended=%q providerID=%q", p.ProviderInst.Status, p.ProviderInst.IntendedStatus, p.ProviderInst.ProviderID)
		slog.Debug("reconcile: entering provider_dead path", "component", "reconcile", "instance", ci.ID, "inst", instDescr, "ci_status", ci.Status)
		return r.checkProviderDead(ci, p.ProviderInst, p.R2Client, p.JobState, p.Now, p.PauseTolerant)
	}
	// Definitive live signal: provider returned a non-terminal instance. Reset
	// any in-flight dead-confirm timer so the hysteresis requires a fresh
	// stretch of dead observations rather than inheriting an old one. Without
	// this, an instance that flickers (alive, dead, alive, dead) over
	// confirmTime would be marked dead on the second dead observation.
	if providerObserved && !providerTerminal {
		r.mu.Lock()
		delete(r.firstDeadAt, ci.ID)
		r.mu.Unlock()
	}

	// 8. Stale heartbeat (display-only warning — the reconciler's probe logic is separate)
	if p.HeartbeatAge > effectiveHeartbeatStaleThreshold(ci.AgentReadyAtUnix, p.Now) && ci.Status == db.LaunchStatusRunning {
		return InstanceAction{
			Kind:         ActionDisplayOnly,
			StallMessage: "heartbeat stale",
		}
	}

	return InstanceAction{Kind: ActionNone}
}

func (r *Reconciler) checkTerminationIntent(ci *db.Launch, inst *cloud.Instance, intent *instanceintent.Marker, _ bool) InstanceAction {
	if intent == nil {
		return InstanceAction{Kind: ActionNone}
	}

	reason := intent.TerminationReason
	// A missing provider observation is unknown, not proof that the expected
	// destroy already happened. Keep the destroy requirement until either the
	// provider is confirmed gone or the durable intent records success. An
	// exited/stopped/error instance is terminal for execution but may remain
	// billable, so it still requires DestroyInstance.
	destroyProvider := inst == nil || needsProviderDestroy(inst)
	var terminalAt time.Time
	if intent.EffectiveState() == instanceintent.StateSucceeded {
		destroyProvider = false
		if intent.DestroySucceededAtUnix > 0 {
			terminalAt = time.Unix(intent.DestroySucceededAtUnix, 0)
		}
	}
	switch intent.TerminalStatus {
	case db.LaunchStatusCompleted:
		if reason == "" {
			reason = db.TerminationReasonCompleted
		}
		return InstanceAction{
			Kind:              ActionTerminationIntent,
			TerminalStatus:    db.LaunchStatusCompleted,
			TerminalAt:        terminalAt,
			TerminationReason: reason,
			DestroyProvider:   destroyProvider,
			AttemptOutcome:    db.AttemptOutcomeCompleted,
		}
	case db.LaunchStatusFailed:
		if reason == "" {
			reason = db.TerminationReasonJobFailure
		}
		return InstanceAction{
			Kind:              ActionTerminationIntent,
			TerminalStatus:    db.LaunchStatusFailed,
			TerminalAt:        terminalAt,
			TerminationReason: reason,
			StallMessage:      strings.TrimSpace(intent.Detail),
			DestroyProvider:   destroyProvider,
			ResetJobs:         true,
			AttemptOutcome:    db.AttemptOutcomeOrphaned,
		}
	case db.LaunchStatusCancelled:
		if reason == "" {
			reason = db.TerminationReasonCancelled
		}
		return InstanceAction{
			Kind:              ActionTerminationIntent,
			TerminalStatus:    db.LaunchStatusCancelled,
			TerminalAt:        terminalAt,
			TerminationReason: reason,
			DestroyProvider:   destroyProvider,
			ResetJobs:         true,
			AttemptOutcome:    db.AttemptOutcomeCancelled,
		}
	default:
		return InstanceAction{Kind: ActionNone}
	}
}

func (r *Reconciler) checkProviderDead(ci *db.Launch, inst *cloud.Instance, r2Client *r2.Client, jobState JobState, now time.Time, pauseTolerant bool) InstanceAction {
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
			DestroyProvider:   needsProviderDestroy(inst),
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
			DestroyProvider:   needsProviderDestroy(inst),
		}
	}

	// Use "unknown" as the default reason — we can't determine what happened.
	// R2 markers may override below with a more specific reason.
	reason := db.TerminationReasonUnknown
	outcome := db.AttemptOutcomeOrphaned
	if ci.LaunchedAt != nil {
		if pauseTolerant && inst != nil && inst.Status == cloud.ProviderStatusExited {
			reason = db.TerminationReasonPreempted
			outcome = db.AttemptOutcomePreempted
		} else {
			reason = db.TerminationReasonProviderFailure
		}
	}
	reasonAfterR2 := failureTerminationReasonFromR2(context.Background(), r2Client, ci.ID, reason)
	if reasonAfterR2 != reason && reasonAfterR2 != db.TerminationReasonPreempted {
		outcome = db.AttemptOutcomeOrphaned
	}
	detail := buildProviderDeadDetail(ci, inst, reason, reasonAfterR2, now)

	slog.Warn("provider dead: no completion marker or termination intent found, resetting jobs",
		"component", "reconcile", "instance", ci.ID, "reason", reasonAfterR2, "detail", detail)
	return InstanceAction{
		Kind:              ActionProviderDead,
		TerminalStatus:    db.LaunchStatusFailed,
		TerminationReason: reasonAfterR2,
		StallMessage:      detail,
		ResetJobs:         true,
		AttemptOutcome:    outcome,
		DestroyProvider:   needsProviderDestroy(inst),
	}
}

// buildProviderDeadDetail summarises the evidence the classifier had at the
// point where it marked an instance failed without a completion marker or
// termination intent. Intended for the launches.termination_detail column
// and operator-facing logs: the post-mortem otherwise has nothing to say
// beyond "termination_reason=unknown".
func buildProviderDeadDetail(ci *db.Launch, inst *cloud.Instance, baseReason, finalReason string, now time.Time) string {
	parts := []string{"provider dead with no completion or intent marker"}
	if inst == nil {
		parts = append(parts, "provider returned no instance")
	} else if inst.Status == "" {
		parts = append(parts, "provider status empty")
	} else {
		parts = append(parts, fmt.Sprintf("provider status=%s", inst.Status))
	}
	if ci != nil && ci.LaunchedAt != nil {
		parts = append(parts, fmt.Sprintf("launched %s ago", now.Sub(time.Unix(*ci.LaunchedAt, 0)).Truncate(time.Second)))
	}
	if ci != nil && ci.AgentReadyAtUnix != nil {
		parts = append(parts, fmt.Sprintf("agent ready %s ago", now.Sub(time.Unix(*ci.AgentReadyAtUnix, 0)).Truncate(time.Second)))
	} else {
		parts = append(parts, "agent never reported ready")
	}
	if finalReason != baseReason {
		parts = append(parts, fmt.Sprintf("R2 marker refined reason %s→%s", baseReason, finalReason))
	} else {
		parts = append(parts, "no R2 disk-failure marker")
	}
	return strings.Join(parts, "; ")
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
	var destroyIntent *instanceintent.Marker

	if action.DestroyProvider {
		if providerID == "" {
			slog.Warn("cannot destroy instance without provider instance ID; deferring terminal status", "component", "reconcile", "instance", ci.ID, "provider", ci.Provider)
			return false, false
		}
		if client == nil {
			slog.Warn("cannot destroy instance without provider client; deferring terminal status", "component", "reconcile", "instance", ci.ID, "provider", ci.Provider)
			return false, false
		}
		var err error
		destroyIntent, err = db.RecordLaunchDestroyIntent(database, ci.ID, action.TerminalStatus, action.TerminationReason)
		if err != nil {
			slog.Warn("failed to record provider destroy intent; deferring destroy", "component", "reconcile", "instance", ci.ID, "error", err)
			return false, false
		}
		if err := client.DestroyInstance(providerID); err != nil && !errors.Is(err, cloud.ErrInstanceNotFound) {
			slog.Warn("failed to destroy instance, deferring terminal status to next reconcile pass", "component", "reconcile", "instance", ci.ID, "provider", providerID, "error", err)
			return false, false
		}
		destroyIntent.State = instanceintent.StateSucceeded
		destroyIntent.DestroySucceededAtUnix = time.Now().Unix()
	}

	if action.TerminalStatus != "" {
		resetCount, err := db.ApplyLaunchTerminalTransition(database, ci.ID, db.LaunchTerminalTransition{
			Status:            action.TerminalStatus,
			EndedAt:           action.TerminalAt,
			TerminationReason: action.TerminationReason,
			TerminationDetail: action.StallMessage,
			ResetJobs:         action.ResetJobs,
			AttemptOutcome:    action.AttemptOutcome,
			TerminationIntent: destroyIntent,
		})
		if err != nil {
			slog.Warn("failed to update instance status", "component", "reconcile", "instance", ci.ID, "status", action.TerminalStatus, "error", err)
			return false, false
		}
		if resetCount > 0 {
			slog.Debug("reset jobs from instance to unplaced", "component", "reconcile", "count", resetCount, "instance", ci.ID)
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

	return true, IsInstanceTerminal(action.TerminalStatus)
}

// String returns a stable name for use in oplog details, slog fields,
// and `%v` formatting. Falls back to the numeric form for unknown values.
func (k InstanceActionKind) String() string {
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
	case ActionIdleAfterReadyTimeout:
		return "idle_after_ready_timeout"
	case ActionProviderDead:
		return "provider_dead"
	case ActionSelfDestructFailed:
		return "self_destruct_failed"
	case ActionPause:
		return "pause"
	case ActionResume:
		return "resume"
	case ActionHedgeCull:
		return "hedge_cull"
	case ActionTerminalLivePhase:
		return "terminal_live_phase"
	case ActionStaleHeartbeat:
		return "stale_heartbeat"
	default:
		return fmt.Sprintf("kind_%d", int(k))
	}
}

// formatActionDetail formats the per-action oplog detail string. Includes
// the provider-status snapshot captured by CheckInstance so post-mortems
// can identify status-classification bugs without re-fetching from the
// provider. Empty fields are omitted.
func formatActionDetail(launchID int64, action InstanceAction) string {
	parts := []string{fmt.Sprintf("launch_id=%d", launchID), fmt.Sprintf("action=%s", action.Kind)}
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
	case ActionHedgeCull:
		return db.EventReconcileHedgeCull
	case ActionTerminalLivePhase:
		return db.EventReconcileRunningStall
	case ActionStaleHeartbeat:
		return db.EventReconcileStaleHeartbeat
	default:
		return ""
	}
}
