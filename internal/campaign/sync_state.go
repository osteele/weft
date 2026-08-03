package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// Test-mockable fetch functions for SyncInstanceState.
//
// syncCheckOnStartProbe checks whether the OnStart probe key exists in R2.
// The live binding is reconcileObjectExists from reconcile.go, which already
// handles nil clients and recovers from stub-client panics in tests. We share
// that binding so the watch and reconcile paths agree on what "probe present"
// means.
var (
	syncFetchInstancePhase   = fetchInstancePhase
	syncFetchBootstrapStage  = fetchBootstrapStage
	syncFetchOnStartStage    = fetchOnStartStage
	syncFetchHeartbeat       = fetchHeartbeat
	syncFetchJobProgress     = fetchJobProgress
	syncFetchTermIntent      = fetchTerminationIntentFromR2
	syncCheckR2GraceStatus   = checkR2GraceStatus
	syncCheckGraceCommandAck = controlplane.CheckGraceCommandAck
	syncCheckOnStartProbe    = func(ctx context.Context, c *r2.Client, key string) (bool, error) {
		return reconcileObjectExists(ctx, c, key)
	}
)

// SyncedState holds the observation state collected from R2 and the provider,
// persisted to launch_live_state, and returned for immediate use.
type SyncedState struct {
	RawInstancePhase      string
	InstancePhase         string
	BootstrapStage        string
	HeartbeatAge          time.Duration
	Heartbeat             *HeartbeatSample
	JobProgress           int // 0-100, or -1 if unavailable
	JobProgressID         int64
	JobProgressPhase      int // 1-based phase number (0 = unknown)
	TerminationIntent     *instanceintent.Marker
	AgentVersion          string
	PhaseChangedAt        *time.Time
	BootstrapActivitySeen bool
	// OnStartProbePresent reports whether the OnStart probe key was seen
	// in R2 during the dud-detection window. True on R2 error (presumed
	// present) so a hiccup cannot combine with other empty signals to
	// false-fire rule 4d. See instance_check.go § rule 4d for the wider
	// absence-conjunction contract and lab-notebook EXP-021 for context.
	OnStartProbePresent bool
	JobsUpdated         int // count of job status transitions (e.g. queued→running)
	// OnStartStage and OnStartStageChangedAt mirror the OnStart shell's
	// stage marker and its R2 last-modified time. Populated only in the
	// pre-bootstrap.sh window (no job phase, no started job), the same window
	// the bootstrap-stage fetch covers. See instance_check.go rules 4e/4f.
	OnStartStage          string
	OnStartStageChangedAt *time.Time
}

// SyncInstanceStateOpts configures optional behaviors for SyncInstanceState.
type SyncInstanceStateOpts struct {
	// AgentVersion is passed in if already known (cached by caller).
	// If empty, SyncInstanceState fetches it from R2.
	AgentVersion string
	// AgentVersionFetched indicates the caller already attempted to fetch
	// the agent version (even if the result was empty).
	AgentVersionFetched bool
}

// SyncInstanceState polls external sources (R2 markers) for a running cloud
// instance and persists the observed state to launch_live_state. It also
// performs side effects: marking queued jobs as running, detecting grace
// transitions, and persisting termination intents.
//
// Returns the synced state for immediate use by the caller (e.g., to build
// CheckInstanceParams or InstanceUpdate).
func SyncInstanceState(
	ctx context.Context,
	database *sql.DB,
	ci *db.Launch,
	r2Client *r2.Client,
	jobs []*db.Job,
	jobState JobState,
	opts SyncInstanceStateOpts,
) *SyncedState {
	s := &SyncedState{
		JobProgress: -1,
	}

	if r2Client == nil || (ci.Status != db.LaunchStatusRunning && ci.Status != db.LaunchStatusGrace) {
		return s
	}

	instanceID := ci.ID
	reconcilePendingMoveTargetRequestAcks(ctx, database, r2Client, instanceID)
	reconcileAbandonedMoveTargetAttempts(ctx, database, r2Client, instanceID)

	if opts.AgentVersionFetched {
		s.AgentVersion = opts.AgentVersion
	}

	// Wave 1: independent R2 markers. Best-effort; failures don't fail others.
	var (
		wave1         sync.WaitGroup
		fetchedIntent *instanceintent.Marker
		intentErr     error
	)
	wave1.Add(2)
	go func() {
		defer wave1.Done()
		fetchedIntent, intentErr = syncFetchTermIntent(ctx, r2Client, instanceID)
	}()
	go func() {
		defer wave1.Done()
		s.RawInstancePhase = syncFetchInstancePhase(ctx, r2Client, instanceID)
	}()
	if !opts.AgentVersionFetched {
		wave1.Add(1)
		go func() {
			defer wave1.Done()
			s.AgentVersion, _, _ = fetchR2Marker(ctx, r2Client, r2keys.InstanceAgentVersion(instanceID))
		}()
	}
	wave1.Wait()

	if intentErr == nil && fetchedIntent != nil {
		s.TerminationIntent = fetchedIntent
		_ = db.UpdateLaunchTerminationIntent(database, instanceID, fetchedIntent)
		ci.TerminationIntent = fetchedIntent
	} else if ci.TerminationIntent != nil {
		s.TerminationIntent = ci.TerminationIntent
	}

	if verb, rawJobID, ok := ParsePhaseJobID(s.RawInstancePhase); ok && rawJobID > 0 && phaseAdoptsMoveTarget(verb) {
		adoptObservedMoveTarget(ctx, database, r2Client, instanceID, rawJobID)
	}

	if freshJobs, err := db.GetLaunchJobsIncludingAttempts(database, instanceID); err == nil {
		jobs = freshJobs
		for _, job := range jobs {
			if jobStartedOnInstance(job) {
				jobState.HasStartedJob = true
				break
			}
		}
	} else {
		slog.Warn("refresh launch jobs for instance phase", "component", "sync", "instance", instanceID, "error", err)
	}

	jobStatuses := make(map[int64]string, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		jobStatuses[job.ID] = job.Status
	}
	s.InstancePhase, _ = displayPhase(jobStatuses, jobs, s.RawInstancePhase)

	// Wave 2: phase-dependent markers. Each goroutine writes to a disjoint
	// field of s; publication happens via wave2.Wait().
	var wave2 sync.WaitGroup
	phaseNonEmpty := s.InstancePhase != ""
	phaseActive := isActiveInstancePhase(s.InstancePhase)
	if phaseActive {
		wave2.Add(1)
		go func() {
			defer wave2.Done()
			s.JobProgressID, s.JobProgress, s.JobProgressPhase = syncFetchJobProgress(ctx, r2Client, s.InstancePhase, jobs)
		}()
	} else if !jobState.HasStartedJob {
		wave2.Add(1)
		go func() {
			defer wave2.Done()
			s.BootstrapStage = syncFetchBootstrapStage(ctx, r2Client, instanceID)
		}()
		wave2.Add(1)
		go func() {
			defer wave2.Done()
			s.OnStartStage, s.OnStartStageChangedAt = syncFetchOnStartStage(ctx, r2Client, instanceID)
		}()
	}
	// In the dud-detection window we also fetch the heartbeat and OnStart
	// probe — both are positive "agent alive" signals the watchdog needs
	// to weigh against its absence-conjunction. See EXP-021.
	inDudWindow := ci.InDudDetectionWindow()
	if phaseNonEmpty || jobState.HasStartedJob || inDudWindow {
		wave2.Add(1)
		go func() {
			defer wave2.Done()
			s.Heartbeat, s.HeartbeatAge = syncFetchHeartbeat(ctx, r2Client, instanceID)
		}()
	}
	var probeExists bool
	var probeErr error
	probeChecked := r2Client != nil && inDudWindow
	if probeChecked {
		wave2.Add(1)
		go func() {
			defer wave2.Done()
			probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			probeExists, probeErr = syncCheckOnStartProbe(probeCtx, r2Client, r2keys.InstanceOnStartProbe(instanceID))
		}()
	}
	wave2.Wait()
	if probeChecked {
		// Presume present on R2 error (see OnStartProbePresent doc).
		// Only confirmed-present observations are persisted, so the
		// future survival training data stays uncontaminated.
		if probeErr != nil {
			s.OnStartProbePresent = true
		} else {
			s.OnStartProbePresent = probeExists
			if probeExists && ci.FirstOnStartProbeSeenUnix == nil {
				now := time.Now()
				if persistErr := db.SetLaunchFirstOnStartProbeSeenIfUnset(database, instanceID, now); persistErr != nil {
					slog.Warn("persist first onstart-probe-seen", "component", "sync", "instance", instanceID, "error", persistErr)
				} else {
					t := now.Unix()
					ci.FirstOnStartProbeSeenUnix = &t
				}
			}
		}
	}

	if hbPhase := freshHeartbeatPhase(ci, s.Heartbeat, s.HeartbeatAge); hbPhase != "" && hbPhase != s.InstancePhase {
		if phase, _ := displayPhase(jobStatuses, jobs, hbPhase); phase != "" {
			s.InstancePhase = phase
			phaseNonEmpty = true
			s.JobProgressID, s.JobProgress, s.JobProgressPhase = syncFetchJobProgress(ctx, r2Client, s.InstancePhase, jobs)
		}
	}

	// First-ready signal: persist agent_ready_at_unix the first time
	// either the bootstrap stage flips to "ready" or any job has started
	// running on this launch. The DB update is idempotent (the IfUnset
	// guard means subsequent calls are no-ops).
	if ci != nil && ci.AgentReadyAtUnix == nil &&
		(s.BootstrapStage == bootstrapStageReady || jobState.HasStartedJob) {
		if err := db.SetLaunchAgentReadyAtIfUnset(database, instanceID, time.Now()); err == nil {
			now := time.Now().Unix()
			ci.AgentReadyAtUnix = &now
			confirmMoveIntentsForReadyLaunch(ctx, database, r2Client, instanceID)
		}
	}

	if phaseNonEmpty {
		// Mark queued jobs as active if the R2 phase says they are in
		// setup or execution, and re-associate orphaned jobs with this
		// launch.
		if verb, phaseJobID, ok := ParsePhaseJobID(s.InstancePhase); ok && phaseJobID > 0 {
			if phaseAdoptsMoveTarget(verb) {
				adoptObservedMoveTarget(ctx, database, r2Client, instanceID, phaseJobID)
				j := findJobInSlice(jobs, phaseJobID)
				if j == nil {
					var err error
					j, err = db.GetJobByID(database, phaseJobID)
					if err != nil && err != sql.ErrNoRows {
						slog.Warn("failed to refresh phase job", "component", "sync", "job_id", phaseJobID, "error", err)
					}
				}
				if j != nil {
					if verb == PhaseSetup || verb == PhaseGPUWarmup {
						if j.Status == db.StatusQueued || j.Status == db.StatusPendingPlacement {
							if err := db.MarkQueuedJobStarting(database, phaseJobID); err != nil {
								slog.Warn("failed to mark job starting from R2 phase", "component", "sync", "job_id", phaseJobID, "error", err)
							} else {
								s.JobsUpdated++
							}
						}
					} else if j.Status == db.StatusQueued || j.Status == db.StatusPendingPlacement || j.Status == db.StatusStarting {
						if err := db.MarkQueuedJobRunning(database, phaseJobID); err != nil {
							slog.Warn("failed to mark job running from R2 phase", "component", "sync", "job_id", phaseJobID, "error", err)
						} else {
							s.JobsUpdated++
						}
					}
					// Re-link orphaned job to this launch if its attempt
					// was reset (no launch_id) but the agent is still running it.
					if j.LaunchID == nil || *j.LaunchID != instanceID {
						if err := db.SetAttemptLaunch(database, phaseJobID, instanceID); err != nil {
							slog.Warn("failed to re-associate job with launch", "component", "sync", "job_id", phaseJobID, "launch_id", instanceID, "error", err)
						}
					}
					jobState.HasStartedJob = true
				}
			}
		}

		if ci != nil && ci.AgentReadyAtUnix == nil && jobState.HasStartedJob {
			if err := db.SetLaunchAgentReadyAtIfUnset(database, instanceID, time.Now()); err == nil {
				now := time.Now().Unix()
				ci.AgentReadyAtUnix = &now
				confirmMoveIntentsForReadyLaunch(ctx, database, r2Client, instanceID)
			}
		}

		// Detect grace transition
		if s.InstancePhase == PhaseGrace && ci.Status == db.LaunchStatusRunning {
			if hasActiveLaunchJobs(jobs, nil) {
				slog.Debug("ignoring grace transition while launch has active jobs", "component", "sync", "instance", ci.ID)
			} else if syncCheckR2GraceStatus(r2Client, ci, database) {
				ci.Status = db.LaunchStatusGrace
			}
		}

		// Detect grace→running transition (agent picked up resubmitted jobs)
		if ci.Status == db.LaunchStatusGrace && s.InstancePhase != PhaseGrace {
			if err := db.ClearLaunchGrace(database, ci.ID); err != nil {
				slog.Warn("failed to clear grace for instance", "component", "sync", "instance", ci.ID, "error", err)
			} else {
				slog.Info("instance exited grace — agent running resubmitted jobs", "component", "sync", "instance", ci.ID, "phase", s.InstancePhase)
				ci.Status = db.LaunchStatusRunning
			}
		}
	}

	if _, currentJobID, ok := ParsePhaseJobID(s.InstancePhase); ok && currentJobID > 0 {
		for _, j := range jobs {
			if j.ID != currentJobID && j.Status == db.StatusRunning {
				CheckAndSyncJobComplete(ctx, r2Client, database, j.ID)
				break // re-evaluate on next poll
			}
		}
	}

	// Instance-scoped convergence: once a launch is active, stale
	// pending_placement intents should be normalized back to queued.
	if updated, err := db.NormalizePendingPlacementForLaunch(database, instanceID); err != nil {
		slog.Warn("failed to normalize pending placement jobs", "component", "sync", "instance", instanceID, "error", err)
	} else if updated > 0 {
		s.JobsUpdated += int(updated)
	}

	var hbJSON string
	var hbTS int64
	if s.Heartbeat != nil {
		if data, err := json.Marshal(s.Heartbeat); err == nil {
			hbJSON = string(data)
		}
		hbTS = s.Heartbeat.Ts
	}
	previousBootstrapStage := ""
	live, err := db.GetLaunchLiveState(database, ci.ID)
	if err != nil {
		slog.Warn("read launch live state", "component", "sync", "instance", instanceID, "error", err)
	} else if live != nil {
		previousBootstrapStage = strings.TrimSpace(live.BootstrapStage)
	}
	s.BootstrapActivitySeen = previousBootstrapStage != "" || strings.TrimSpace(s.BootstrapStage) != ""
	if !s.BootstrapActivitySeen {
		seen, err := db.HasBootstrapTransitions(database, ci.ID)
		if err != nil {
			slog.Warn("check bootstrap transitions", "component", "sync", "instance", instanceID, "error", err)
			// Presume seen on DB error — rule 4d's absence conjunction
			// must not fire on a SQLite hiccup. Mirrors OnStart probe.
			s.BootstrapActivitySeen = true
		} else {
			s.BootstrapActivitySeen = seen
		}
	}

	if syncObservedR2State(s) {
		phaseChangedAt, _ := db.UpsertLaunchLiveState(database, db.LaunchLiveState{
			LaunchID:         instanceID,
			InstancePhase:    s.InstancePhase,
			BootstrapStage:   s.BootstrapStage,
			HeartbeatJSON:    hbJSON,
			HeartbeatTS:      hbTS,
			JobProgressPct:   s.JobProgress,
			JobProgressID:    s.JobProgressID,
			JobProgressPhase: s.JobProgressPhase,
			AgentVersion:     s.AgentVersion,
		})
		if phaseChangedAt != nil {
			t := time.Unix(*phaseChangedAt, 0)
			s.PhaseChangedAt = &t
		}
	}
	extendBootstrapDeadlineFromProgress(database, ci, s.BootstrapStage, previousBootstrapStage)

	return s
}

func phaseAdoptsMoveTarget(verb string) bool {
	switch verb {
	case PhaseSetup, PhaseGPUWarmup, PhaseRunning, PhaseUploading, PhaseUploadingResults, PhaseFinalizing:
		return true
	default:
		return false
	}
}

// RefreshLaunchPhasesFromDB refreshes the cached display phase for live
// launches without provider observations. It is intentionally read-only with
// respect to provider state and must not call CheckInstance or termination
// logic; it exists so database-observed running jobs can correct stale
// launch_live_state.instance_phase even when cloud provider clients or the
// cloud-sync lease are unavailable.
func RefreshLaunchPhasesFromDB(ctx context.Context, database *sql.DB, r2Client *r2.Client) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if database == nil {
		return 0, nil
	}
	launches, err := db.ListRunningLaunches(database)
	if err != nil {
		return 0, err
	}
	updated := 0
	for _, launch := range launches {
		if launch == nil {
			continue
		}
		jobs, err := db.GetLaunchJobsIncludingAttempts(database, launch.ID)
		if err != nil {
			slog.Warn("refresh phase jobs", "component", "sync", "instance", launch.ID, "error", err)
			continue
		}
		observedPhase := ""
		if r2Client != nil && (launch.Status == db.LaunchStatusRunning || launch.Status == db.LaunchStatusGrace) {
			observedPhase = syncFetchInstancePhase(ctx, r2Client, launch.ID)
		}
		if observedPhase == "" {
			if live, err := db.GetLaunchLiveState(database, launch.ID); err == nil && live != nil {
				observedPhase = strings.TrimSpace(live.InstancePhase)
			} else if err != nil {
				slog.Warn("read live phase for refresh", "component", "sync", "instance", launch.ID, "error", err)
			}
		}
		phase, _ := DisplayPhase(jobs, observedPhase)
		if phase == "" {
			continue
		}
		if changedAt, err := db.SetLaunchLiveInstancePhase(database, launch.ID, phase); err != nil {
			slog.Warn("refresh launch phase", "component", "sync", "instance", launch.ID, "phase", phase, "error", err)
		} else if changedAt != nil {
			updated++
		}
	}
	return updated, nil
}

func confirmMoveIntentsForReadyLaunch(ctx context.Context, database *sql.DB, r2Client *r2.Client, instanceID int64) {
	sourceStops, err := db.ListMoveIntentSourceStopsForTargetLaunch(database, instanceID)
	if err != nil {
		slog.Warn("list move source stops for ready launch", "component", "sync", "instance", instanceID, "error", err)
	}
	if err := db.ConfirmOpenMoveIntentsForTargetLaunch(database, instanceID, "target agent_ready"); err != nil {
		slog.Warn("confirm move intents for ready launch", "component", "sync", "instance", instanceID, "error", err)
		return
	}
	stopMoveIntentSources(ctx, database, r2Client, sourceStops)
}

func adoptObservedMoveTarget(ctx context.Context, database *sql.DB, r2Client *r2.Client, instanceID int64, jobID int64) {
	intent, err := db.GetOpenMoveIntent(database, jobID)
	if err != nil {
		slog.Warn("read open move intent for phase job", "component", "sync", "job_id", jobID, "instance", instanceID, "error", err)
		return
	}
	if intent == nil || intent.TargetLaunchID == nil || *intent.TargetLaunchID != instanceID {
		return
	}
	if intent.TargetAttemptID == nil {
		slog.Warn("phase job has open move intent without target attempt", "component", "sync", "job_id", jobID, "instance", instanceID, "intent_id", intent.ID)
		return
	}

	var sourceStops []db.MoveIntentSourceStop
	stops, err := db.ListMoveIntentSourceStopsForTargetLaunch(database, instanceID)
	if err != nil {
		slog.Warn("list move source stops for observed phase", "component", "sync", "job_id", jobID, "instance", instanceID, "intent_id", intent.ID, "error", err)
	} else {
		for _, stop := range stops {
			if stop.IntentID == intent.ID {
				sourceStops = append(sourceStops, stop)
			}
		}
	}
	if err := db.ConfirmMoveTargetAccepted(database, intent.ID, fmt.Sprintf("target phase reported %s", ids.FormatJobID(jobID))); err != nil {
		slog.Warn("confirm move intent from phase job", "component", "sync", "job_id", jobID, "instance", instanceID, "intent_id", intent.ID, "error", err)
		return
	}
	stopMoveIntentSources(ctx, database, r2Client, sourceStops)
}

// abandonedMoveCancelWindow bounds how long sync keeps re-sending
// cancel-attempts markers for recently abandoned move targets. Markers are
// idempotent (the agent unions attempt IDs), so repeats within the window are
// harmless; the window just keeps steady-state syncs from scanning all
// history.
const abandonedMoveCancelWindow = 30 * time.Minute

// reconcileAbandonedMoveTargetAttempts tells the target agent to drop hidden
// move-target attempts whose intent was resolved canceled/obsoleted before
// the attempt started. Without this, a jobs request parked in the target's R2
// queue can start an attempt the DB already canceled, which the stall
// detector then answers by terminating the whole instance.
func reconcileAbandonedMoveTargetAttempts(ctx context.Context, database *sql.DB, r2Client *r2.Client, instanceID int64) {
	since := time.Now().Add(-abandonedMoveCancelWindow).Unix()
	attemptIDs, err := db.ListAbandonedMoveTargetCancelAttempts(database, instanceID, since)
	if err != nil {
		slog.Warn("list abandoned move target attempts", "component", "sync", "instance", instanceID, "error", err)
		return
	}
	if len(attemptIDs) == 0 {
		return
	}
	if err := sendGraceCancelAttempts(ctx, r2Client, instanceID, attemptIDs); err != nil {
		if eventErr := RecordCancelAttemptsDeliveryFailure(database, instanceID, attemptIDs, "abandoned move targets", err); eventErr != nil {
			slog.Warn("record cancel-attempts delivery failure",
				"component", "sync", "instance", instanceID, "attempt_ids", attemptIDs, "error", eventErr)
		}
		slog.Warn("send cancel-attempts for abandoned move targets",
			"component", "sync", "instance", instanceID, "attempt_ids", attemptIDs, "error", err)
	}
}

func reconcilePendingMoveTargetRequestAcks(ctx context.Context, database *sql.DB, r2Client *r2.Client, instanceID int64) {
	pending, err := db.ListOpenMoveIntentPendingTargetRequests(database, instanceID)
	if err != nil {
		slog.Warn("list pending move target requests", "component", "sync", "instance", instanceID, "error", err)
		return
	}
	for _, req := range pending {
		ack, found, err := syncCheckGraceCommandAck(ctx, r2Client, instanceID, req.RequestID)
		if err != nil {
			slog.Warn("check move target request ack",
				"component", "sync", "instance", instanceID, "intent_id", req.IntentID,
				"request_id", req.RequestID, "error", err)
			continue
		}
		if !found {
			continue
		}
		if ack == nil || ack.Kind != controlplane.GraceCommandJobs {
			slog.Warn("ignore unexpected move target ack",
				"component", "sync", "instance", instanceID, "intent_id", req.IntentID,
				"request_id", req.RequestID, "kind", ackKind(ack))
			continue
		}
		if !ack.Accepted {
			resolution := strings.TrimSpace(ack.Message)
			if resolution == "" {
				resolution = "target rejected jobs request"
			} else {
				resolution = "target rejected jobs request: " + resolution
			}
			if err := db.ResolveMoveIntent(database, req.IntentID, db.MoveIntentStateCanceled, resolution); err != nil {
				slog.Warn("cancel rejected move target request",
					"component", "sync", "instance", instanceID, "intent_id", req.IntentID, "error", err)
			}
			continue
		}

		stop := db.MoveIntentSourceStop{
			IntentID:        req.IntentID,
			JobID:           req.JobID,
			SourceAttemptID: req.SourceAttemptID,
			SourceLaunchID:  req.SourceLaunchID,
			SourceHost:      req.SourceHost,
			SourceStartTime: req.SourceStartTime,
		}
		if err := db.ConfirmMoveTargetAccepted(database, req.IntentID, fmt.Sprintf("target accepted request %s", req.RequestID)); err != nil {
			slog.Warn("confirm acknowledged move target request",
				"component", "sync", "instance", instanceID, "intent_id", req.IntentID, "error", err)
			continue
		}
		stopMoveIntentSources(ctx, database, r2Client, []db.MoveIntentSourceStop{stop})
	}
}

func ackKind(ack *controlplane.GraceCommandAck) controlplane.GraceCommandKind {
	if ack == nil {
		return ""
	}
	return ack.Kind
}

func stopMoveIntentSources(ctx context.Context, database *sql.DB, r2Client *r2.Client, sourceStops []db.MoveIntentSourceStop) {
	if len(sourceStops) == 0 {
		return
	}
	cancelByLaunch := map[int64][]int64{}
	for _, stop := range sourceStops {
		if stop.SourceLaunchID != nil && *stop.SourceLaunchID > 0 && stop.SourceAttemptID > 0 {
			sourceLaunchID := *stop.SourceLaunchID
			cancelByLaunch[sourceLaunchID] = append(cancelByLaunch[sourceLaunchID], stop.SourceAttemptID)
			reset, err := ResetStrandedCloudAfterConsumers(database, stop.JobID, sourceLaunchID)
			if err != nil {
				slog.Warn("reset stranded cloud-after consumers after target ready",
					"component", "sync", "job_id", ids.FormatJobID(stop.JobID), "source_launch_id", sourceLaunchID, "error", err)
			} else if len(reset.AttemptIDs) > 0 {
				cancelByLaunch[sourceLaunchID] = append(cancelByLaunch[sourceLaunchID], reset.AttemptIDs...)
				slog.Info("reset stranded cloud-after consumers after target ready",
					"component", "sync", "job_id", ids.FormatJobID(stop.JobID), "source_launch_id", sourceLaunchID, "consumer_job_ids", reset.JobIDs)
			}
			continue
		}
		if stop.SourceHost == "" || stop.SourceStartTime <= 0 {
			continue
		}
		job := &db.Job{ID: stop.JobID, Host: stop.SourceHost, StartTime: stop.SourceStartTime}
		if err := ops.KillQueueRunnerJob(job, 30*time.Second); err != nil {
			slog.Warn("terminate move source after target ready",
				"component", "sync", "job_id", ids.FormatJobID(stop.JobID), "host", stop.SourceHost, "error", err)
		}
	}
	if r2Client == nil {
		return
	}
	for launchID, attemptIDs := range cancelByLaunch {
		if err := sendGraceCancelAttempts(ctx, r2Client, launchID, attemptIDs); err != nil {
			if eventErr := RecordCancelAttemptsDeliveryFailure(database, launchID, attemptIDs, "move source after target ready", err); eventErr != nil {
				slog.Warn("record cancel-attempts delivery failure",
					"component", "sync", "source_launch_id", launchID, "attempt_ids", attemptIDs, "error", eventErr)
			}
			slog.Warn("send move source cancel-attempts after target ready",
				"component", "sync", "source_launch_id", launchID, "attempt_ids", attemptIDs, "error", err)
		}
	}
}

func syncObservedR2State(s *SyncedState) bool {
	if s == nil {
		return false
	}
	return strings.TrimSpace(s.RawInstancePhase) != "" ||
		strings.TrimSpace(s.InstancePhase) != "" ||
		strings.TrimSpace(s.BootstrapStage) != "" ||
		s.Heartbeat != nil ||
		strings.TrimSpace(s.AgentVersion) != "" ||
		s.TerminationIntent != nil ||
		s.JobProgressID > 0 ||
		s.JobProgress >= 0
}

func freshHeartbeatPhase(ci *db.Launch, heartbeat *HeartbeatSample, age time.Duration) string {
	if heartbeat == nil {
		return ""
	}
	if heartbeat.AgentAlive != nil && !*heartbeat.AgentAlive {
		return ""
	}
	threshold := heartbeatStaleThreshold
	if ci != nil {
		threshold = effectiveHeartbeatStaleThreshold(ci.AgentReadyAtUnix, time.Now())
	}
	if age > threshold {
		return ""
	}
	return strings.TrimSpace(heartbeat.Phase)
}

func extendBootstrapDeadlineFromProgress(database *sql.DB, ci *db.Launch, stage string, previousStage string) {
	stage = strings.TrimSpace(stage)
	if database == nil || ci == nil || ci.ID <= 0 || stage == "" || stage == bootstrapStageReady || ci.AgentReadyAtUnix != nil {
		return
	}
	if stage == strings.TrimSpace(previousStage) {
		return
	}
	stageEnteredAt, err := db.LatestBootstrapStageEnteredAt(database, ci.ID, stage)
	if err != nil {
		slog.Warn("read latest bootstrap stage transition", "component", "sync", "instance", ci.ID, "stage", stage, "error", err)
		return
	}
	if stageEnteredAt <= 0 {
		return
	}

	timeout := BootstrapTerminateTimeout
	if survival, err := db.ComputeBootstrapSurvival(database, ci.Provider); err != nil {
		slog.Debug("compute bootstrap survival deadline", "component", "sync", "instance", ci.ID, "provider", ci.Provider, "error", err)
	} else if survival != nil && survival.TerminateAfter > 0 {
		timeout = survival.TerminateAfter
	}

	deadline := time.Unix(stageEnteredAt, 0).Add(timeout)
	if ci.BootstrapDeadlineUnix != nil && *ci.BootstrapDeadlineUnix >= deadline.Unix() {
		return
	}
	if err := db.SetLaunchBootstrapDeadline(database, ci.ID, deadline); err != nil {
		slog.Warn("extend bootstrap deadline", "component", "sync", "instance", ci.ID, "stage", stage, "error", err)
		return
	}
	deadlineUnix := deadline.Unix()
	ci.BootstrapDeadlineUnix = &deadlineUnix
}

// CheckParams builds CheckInstanceParams from synced state and caller-provided context.
// The caller is responsible for setting ProviderInst, ProviderErr,
// ProviderStatusUnknownFor, BootstrapSurvival, and SetupSurvival on the
// returned params.
func (s *SyncedState) CheckParams(ci *db.Launch, r2Client *r2.Client, jobState JobState, now time.Time) CheckInstanceParams {
	instancePhase := s.InstancePhase
	if !isActiveInstancePhase(instancePhase) {
		instancePhase = ""
	}
	return CheckInstanceParams{
		CI:                    ci,
		R2Client:              r2Client,
		JobState:              jobState,
		InstancePhase:         instancePhase,
		BootstrapStage:        s.BootstrapStage,
		HeartbeatAge:          s.HeartbeatAge,
		Heartbeat:             s.Heartbeat,
		Now:                   now,
		TerminationIntent:     s.TerminationIntent,
		PhaseChangedAt:        s.PhaseChangedAt,
		BootstrapActivitySeen: s.BootstrapActivitySeen,
		OnStartProbePresent:   s.OnStartProbePresent,
		OnStartStage:          s.OnStartStage,
		OnStartStageChangedAt: s.OnStartStageChangedAt,
	}
}
