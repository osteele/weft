package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// Test-mockable fetch functions for SyncInstanceState.
var (
	syncFetchInstancePhase  = fetchInstancePhase
	syncFetchBootstrapStage = fetchBootstrapStage
	syncFetchHeartbeat      = fetchHeartbeat
	syncFetchJobProgress    = fetchJobProgress
	syncFetchTermIntent     = fetchTerminationIntentFromR2
	syncCheckR2GraceStatus  = checkR2GraceStatus
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
	JobsUpdated           int // count of job status transitions (e.g. queued→running)
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
			s.AgentVersion = fetchR2Marker(ctx, r2Client, r2keys.InstanceAgentVersion(instanceID))
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
	if phaseNonEmpty {
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
	}
	if phaseNonEmpty || jobState.HasStartedJob {
		wave2.Add(1)
		go func() {
			defer wave2.Done()
			s.Heartbeat, s.HeartbeatAge = syncFetchHeartbeat(ctx, r2Client, instanceID)
		}()
	}
	wave2.Wait()

	// First-ready signal: persist agent_ready_at_unix the first time
	// either the bootstrap stage flips to "ready" or any job has started
	// running on this launch. The DB update is idempotent (the IfUnset
	// guard means subsequent calls are no-ops).
	if ci != nil && ci.AgentReadyAtUnix == nil &&
		(s.BootstrapStage == bootstrapStageReady || jobState.HasStartedJob) {
		if err := db.SetLaunchAgentReadyAtIfUnset(database, instanceID, time.Now()); err == nil {
			now := time.Now().Unix()
			ci.AgentReadyAtUnix = &now
			_ = db.ConfirmOpenMoveIntentsForTargetLaunch(database, instanceID, "target agent_ready")
		}
	}

	if phaseNonEmpty {
		// Mark queued jobs as running if the R2 phase says they are,
		// and re-associate orphaned jobs with this launch.
		if verb, phaseJobID, ok := ParsePhaseJobID(s.InstancePhase); ok && phaseJobID > 0 {
			switch verb {
			case PhaseSetup, PhaseRunning, PhaseUploading, PhaseUploadingResults, PhaseFinalizing:
				if j := findJobInSlice(jobs, phaseJobID); j != nil {
					if j.Status == db.StatusQueued {
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

	// Persist to launch_live_state and get resolved PhaseChangedAt.
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
		}
		s.BootstrapActivitySeen = seen
	}
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
	extendBootstrapDeadlineFromProgress(database, ci, s.BootstrapStage, previousBootstrapStage)

	return s
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
// The caller is responsible for setting ProviderInst, ProviderErr, BootstrapSurvival,
// and SetupSurvival on the returned params.
func (s *SyncedState) CheckParams(ci *db.Launch, r2Client *r2.Client, jobState JobState, now time.Time) CheckInstanceParams {
	return CheckInstanceParams{
		CI:                    ci,
		R2Client:              r2Client,
		JobState:              jobState,
		InstancePhase:         s.InstancePhase,
		BootstrapStage:        s.BootstrapStage,
		HeartbeatAge:          s.HeartbeatAge,
		Heartbeat:             s.Heartbeat,
		Now:                   now,
		TerminationIntent:     s.TerminationIntent,
		PhaseChangedAt:        s.PhaseChangedAt,
		BootstrapActivitySeen: s.BootstrapActivitySeen,
	}
}
