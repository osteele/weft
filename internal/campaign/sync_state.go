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

	// 1. Termination intent
	if fetchedIntent, err := syncFetchTermIntent(ctx, r2Client, instanceID); err == nil && fetchedIntent != nil {
		s.TerminationIntent = fetchedIntent
		_ = db.UpdateLaunchTerminationIntent(database, instanceID, fetchedIntent)
		ci.TerminationIntent = fetchedIntent
	} else if ci.TerminationIntent != nil {
		s.TerminationIntent = ci.TerminationIntent
	}

	// 2. Instance phase
	s.InstancePhase = syncFetchInstancePhase(ctx, r2Client, instanceID)
	if s.InstancePhase != "" {
		// Job progress (only meaningful when a phase is active)
		s.JobProgressID, s.JobProgress, s.JobProgressPhase = syncFetchJobProgress(ctx, r2Client, s.InstancePhase, jobs)

		// Mark queued jobs as running if the R2 phase says they are
		if verb, phaseJobID, ok := ParsePhaseJobID(s.InstancePhase); ok && phaseJobID > 0 {
			switch verb {
			case PhaseRunning, PhaseUploading, PhaseUploadingResults, PhaseFinalizing:
				if j := findJobInSlice(jobs, phaseJobID); j != nil && j.Status == db.StatusQueued {
					if err := db.MarkQueuedJobRunning(database, phaseJobID); err != nil {
						slog.Warn("failed to mark job running from R2 phase", "component", "sync", "job_id", phaseJobID, "error", err)
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
	} else if !jobState.HasStartedJob {
		// 3. Bootstrap stage (only when no phase and no jobs started)
		s.BootstrapStage = syncFetchBootstrapStage(ctx, r2Client, instanceID)
	}

	// 4. Heartbeat (when jobs have started or a phase is active)
	if jobState.HasStartedJob || s.InstancePhase != "" {
		s.Heartbeat, s.HeartbeatAge = syncFetchHeartbeat(ctx, r2Client, instanceID)
	}

	// 5. Sync completion for jobs that aren't the current phase job
	if _, currentJobID, ok := ParsePhaseJobID(s.InstancePhase); ok && currentJobID > 0 {
		for _, j := range jobs {
			if j.ID != currentJobID && j.Status == db.StatusRunning {
				CheckAndSyncJobComplete(ctx, r2Client, database, j.ID)
				break // re-evaluate on next poll
			}
		}
	}

	// 6. Agent version
	if opts.AgentVersionFetched {
		s.AgentVersion = opts.AgentVersion
	} else {
		s.AgentVersion = fetchR2Marker(ctx, r2Client, r2keys.InstanceAgentVersion(instanceID))
	}

	// 7. Persist to launch_live_state
	var hbJSON string
	var hbTS int64
	if s.Heartbeat != nil {
		if data, err := json.Marshal(s.Heartbeat); err == nil {
			hbJSON = string(data)
		}
		hbTS = s.Heartbeat.Ts
	}
	_ = db.UpsertLaunchLiveState(database, db.LaunchLiveState{
		LaunchID:       instanceID,
		InstancePhase:  s.InstancePhase,
		BootstrapStage: s.BootstrapStage,
		HeartbeatJSON:  hbJSON,
		HeartbeatTS:    hbTS,
		JobProgressPct: s.JobProgress,
		JobProgressID:  s.JobProgressID,
		AgentVersion:   s.AgentVersion,
	})

	// 8. Read back PhaseChangedAt from the DB (set by UpsertLaunchLiveState)
	if liveState, err := db.GetLaunchLiveState(database, instanceID); err == nil && liveState != nil && liveState.PhaseChangedAt != nil {
		t := time.Unix(*liveState.PhaseChangedAt, 0)
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
