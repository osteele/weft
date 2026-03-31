package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
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
)

// SyncedState holds the observation state collected from R2 and the provider,
// persisted to launch_live_state, and returned for immediate use.
type SyncedState struct {
	InstancePhase     string
	BootstrapStage    string
	HeartbeatAge      time.Duration
	Heartbeat         *HeartbeatSample
	JobProgress       int // 0-100, or -1 if unavailable
	JobProgressID     int64
	JobProgressPhase  int // 1-based phase number (0 = unknown)
	TerminationIntent *instanceintent.Marker
	AgentVersion      string
	PhaseChangedAt    *time.Time
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

	if fetchedIntent, err := syncFetchTermIntent(ctx, r2Client, instanceID); err == nil && fetchedIntent != nil {
		s.TerminationIntent = fetchedIntent
		_ = db.UpdateLaunchTerminationIntent(database, instanceID, fetchedIntent)
		ci.TerminationIntent = fetchedIntent
	} else if ci.TerminationIntent != nil {
		s.TerminationIntent = ci.TerminationIntent
	}

	s.InstancePhase = syncFetchInstancePhase(ctx, r2Client, instanceID)
	if s.InstancePhase != "" {
		// Job progress (only meaningful when a phase is active)
		s.JobProgressID, s.JobProgress, s.JobProgressPhase = syncFetchJobProgress(ctx, r2Client, s.InstancePhase, jobs)

		// Mark queued jobs as running if the R2 phase says they are,
		// and re-associate orphaned jobs with this launch.
		if verb, phaseJobID, ok := ParsePhaseJobID(s.InstancePhase); ok && phaseJobID > 0 {
			switch verb {
			case PhaseRunning, PhaseUploading, PhaseUploadingResults, PhaseFinalizing:
				if j := findJobInSlice(jobs, phaseJobID); j != nil {
					if j.Status == db.StatusQueued {
						if err := db.MarkQueuedJobRunning(database, phaseJobID); err != nil {
							slog.Warn("failed to mark job running from R2 phase", "component", "sync", "job_id", phaseJobID, "error", err)
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
			if checkR2GraceStatus(r2Client, ci, database) {
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
	} else if !jobState.HasStartedJob {
		s.BootstrapStage = syncFetchBootstrapStage(ctx, r2Client, instanceID)
	}

	if jobState.HasStartedJob || s.InstancePhase != "" {
		s.Heartbeat, s.HeartbeatAge = syncFetchHeartbeat(ctx, r2Client, instanceID)
	}

	if _, currentJobID, ok := ParsePhaseJobID(s.InstancePhase); ok && currentJobID > 0 {
		for _, j := range jobs {
			if j.ID != currentJobID && j.Status == db.StatusRunning {
				CheckAndSyncJobComplete(ctx, r2Client, database, j.ID)
				break // re-evaluate on next poll
			}
		}
	}

	if opts.AgentVersionFetched {
		s.AgentVersion = opts.AgentVersion
	} else {
		s.AgentVersion = fetchR2Marker(ctx, r2Client, r2keys.InstanceAgentVersion(instanceID))
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
	phaseChangedAt, _ := db.UpsertLaunchLiveState(database, db.LaunchLiveState{
		LaunchID:       instanceID,
		InstancePhase:  s.InstancePhase,
		BootstrapStage: s.BootstrapStage,
		HeartbeatJSON:  hbJSON,
		HeartbeatTS:    hbTS,
		JobProgressPct: s.JobProgress,
		JobProgressID:  s.JobProgressID,
		AgentVersion:   s.AgentVersion,
	})
	if phaseChangedAt != nil {
		t := time.Unix(*phaseChangedAt, 0)
		s.PhaseChangedAt = &t
	}

	return s
}

// CheckParams builds CheckInstanceParams from synced state and caller-provided context.
// The caller is responsible for setting ProviderInst, ProviderErr, BootstrapSurvival,
// and SetupSurvival on the returned params.
func (s *SyncedState) CheckParams(ci *db.Launch, r2Client *r2.Client, jobState JobState, now time.Time) CheckInstanceParams {
	return CheckInstanceParams{
		CI:                ci,
		R2Client:          r2Client,
		JobState:          jobState,
		InstancePhase:     s.InstancePhase,
		BootstrapStage:    s.BootstrapStage,
		HeartbeatAge:      s.HeartbeatAge,
		Now:               now,
		TerminationIntent: s.TerminationIntent,
		PhaseChangedAt:    s.PhaseChangedAt,
	}
}
