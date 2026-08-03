package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/util"
)

// Bootstrap timeout thresholds.
const (
	bootstrapWarnTimeout      = 15 * time.Minute // warn after this long with no progress
	BootstrapTerminateTimeout = 20 * time.Minute // auto-terminate after this long
	bootstrapStageReady       = "ready"          // R2 marker value when bootstrap is complete

	// launchingPhaseTimeout bounds `launching` when BootstrapOrigin is
	// nil. See campaign-lifecycle.allium config.launching_phase_timeout
	// for calibration and rationale.
	// MUST exceed the launching goroutine's own context timeout so its
	// cancellation fires first; this is the safety net. Matches
	// config.launching_phase_timeout in campaign-lifecycle.allium.
	launchingPhaseTimeout = 12 * time.Minute

	// dudVastTimeout is how long after Vast reports `running` we wait
	// for the OnStart first-line probe to land in R2 before declaring
	// a "dud" — Vast says the container is running, but no agent
	// activity ever appears. The bimodal distribution of probe arrival
	// times observed 2026-05-06 (probe within ~60s OR never) makes
	// this a sharp signal: anything past several minutes is a host-
	// level binary failure, not slow image pull. Set generously enough
	// to cover legitimate cold-image pulls (typically 1–3 min) but
	// well below the adaptive bootstrap deadline (often 1h+) so dud
	// rentals are reclaimed before they bleed budget.
	dudVastTimeout = 8 * time.Minute

	// onStartStallTimeout is how long the OnStart shell's stage marker may
	// sit unchanged on an in-progress step (apt/uv/rclone install) before the
	// chain is declared dead. OnStart install steps normally finish in 1–3
	// min; a marker frozen well past that with no bootstrap.sh handoff is a
	// hung apt mirror / failed install, not slow progress. This closes the
	// gap between dud detection (no probe at all) and the bootstrap `failed:`
	// marker (written only by bootstrap.sh, which never ran in this case),
	// so a stalled OnStart chain is reaped in minutes instead of waiting the
	// full adaptive bootstrap deadline. See instance_check.go rule 4f.
	onStartStallTimeout = 10 * time.Minute

	// onStartTotalActiveTimeout caps the total time an instance may sit in
	// the OnStart phase (probe seen, no bootstrap.sh handoff, no agent ready)
	// regardless of stage-marker freshness. Rule 4f measures a stall from the
	// marker's R2 last-modified, which some providers (e.g. RunPod) refresh by
	// re-running a failed OnStart from the top — the marker looks fresh while
	// the chain loops without ever progressing to bootstrap.sh, so 4f never
	// fires and the instance bleeds budget until the ~2h adaptive bootstrap
	// deadline. Anchoring on FirstOnStartProbeSeenUnix (set-once, churn-immune)
	// bounds that case. Set generously above onStartStallTimeout so a slow but
	// genuinely progressing OnStart is not clipped. See instance_check.go rule 4g.
	onStartTotalActiveTimeout = 25 * time.Minute
)

// Setup phase stall defaults (used when no survival data is available).
const (
	defaultSetupStallWarn      = 25 * time.Minute
	defaultSetupStallTerminate = 45 * time.Minute
)

// Heartbeat staleness thresholds. The base value is the steady-state
// kill threshold; a longer early-life value covers the heavier I/O of
// initial setup (multi-GB HF model downloads, uv sync writes, container
// memory pressure) which has been observed to stall the sidecar's R2
// PUTs even when the container is alive.
//
// Calibration: agents that go silent past 5 minutes have not been
// observed to recover in our data. The early-life carve-out is bounded
// by heartbeatEarlyLifeWindow after agent_ready_at — past that window
// the agent is post-first-job setup and heartbeats reliably during
// steady-state GPU work.
const (
	heartbeatStaleThreshold          = 5 * time.Minute
	heartbeatStaleThresholdEarlyLife = 8 * time.Minute
	heartbeatEarlyLifeWindow         = 15 * time.Minute
)

// effectiveHeartbeatStaleThreshold returns the kill threshold for an
// agent given how recently it reached "ready". Within
// heartbeatEarlyLifeWindow of agent_ready_at_unix the threshold is
// lenient; after, it tightens to the steady-state value.
func effectiveHeartbeatStaleThreshold(agentReadyAtUnix *int64, now time.Time) time.Duration {
	if agentReadyAtUnix == nil || *agentReadyAtUnix <= 0 {
		return heartbeatStaleThreshold
	}
	if now.Sub(time.Unix(*agentReadyAtUnix, 0)) < heartbeatEarlyLifeWindow {
		return heartbeatStaleThresholdEarlyLife
	}
	return heartbeatStaleThreshold
}

// Running-phase stall thresholds: trigger when heartbeat is stale AND
// the running phase has been unchanged for this long. This catches hung
// jobs where the agent has died but the provider still reports "running".
// NOT based on GPU utilization — jobs may legitimately not use the GPU.
const (
	runningStaleWarn      = 20 * time.Minute
	runningStaleTerminate = 60 * time.Minute
)

// HeartbeatSample mirrors the agent's heartbeat JSON payload.
type HeartbeatSample struct {
	Ts             int64   `json:"ts"`
	Phase          string  `json:"phase"`
	GPUUtilPct     int     `json:"gpu_util_pct"`
	GPUMemUsedMiB  int     `json:"gpu_mem_used_mib"`
	GPUMemTotalMiB int     `json:"gpu_mem_total_mib"`
	GPUTempC       int     `json:"gpu_temp_c"`
	HostRSSKB      int64   `json:"host_rss_kb"`
	HostMemTotalKB int64   `json:"host_mem_total_kb"`
	LoadAvg1       float64 `json:"load_avg_1,omitempty"`
	CPUCount       int     `json:"cpu_count,omitempty"`
	DiskFreeBytes  int64   `json:"disk_free_bytes"`
	DiskTotalBytes int64   `json:"disk_total_bytes"`
	AgentPID       int     `json:"agent_pid,omitempty"`
	AgentAlive     *bool   `json:"agent_alive,omitempty"`
	AgentFatal     string  `json:"agent_fatal,omitempty"`
}

// InstanceUpdate is a snapshot of cloud instance + job state.
type InstanceUpdate struct {
	Launch                  *db.Launch
	Jobs                    []*db.Job
	JobPhaseTimings         map[int64]*db.JobPhaseTimings
	JobAttemptOutcomes      map[int64]string // job_id → attempt outcome for this instance
	Instance                *cloud.Instance  // nil if not yet provisioned
	BootstrapStage          string           // current bootstrap stage from R2 (e.g. "agent_installed")
	RawInstancePhase        string           // raw job execution phase from R2 (e.g. "setup:123")
	InstancePhase           string           // reconciled job execution phase for display/checks (e.g. "running:123")
	PhaseChangedAt          *time.Time       // first observed time of the current phase within this watcher
	StallMessage            string           // non-empty if bootstrap appears stuck
	JobProgress             int              // -1 = no progress, 0-100 = raw percent within current phase
	JobProgressID           int64            // which job the progress is for
	JobProgressPhase        int              // 1-based phase number (0 = unknown/single-phase)
	HeartbeatAge            time.Duration    // time since last heartbeat (0 = no heartbeat fetched)
	Heartbeat               *HeartbeatSample // latest heartbeat metrics (nil if unavailable)
	TerminationIntent       *instanceintent.Marker
	BootstrapDurations      db.BootstrapDurations // sorted historical bootstrap durations for conditional estimates
	BootstrapTerminateAfter time.Duration         // learned termination deadline; 0 = use BootstrapTerminateTimeout
}

// AttemptDisplayStatus returns the status to display for a job in the context
// of a specific instance. It prefers the attempt outcome (e.g. "orphaned",
// "completed") from the instance's attempt record over the job's current DB
// status, since the job may have been re-queued for retry on another instance.
func AttemptDisplayStatus(j *db.Job, outcomes map[int64]string) string {
	if outcome, ok := outcomes[j.ID]; ok {
		return outcome
	}
	return j.Status
}

// resolveLastProviderStatusChange looks up the most recent
// provider_status_transitions row for a launch and stores its
// observed_at on the params. A DB error is logged at debug level and
// otherwise swallowed — rule 4b falls back to the lifecycle anchor,
// matching pre-feature behaviour.
func resolveLastProviderStatusChange(database *sql.DB, params *CheckInstanceParams, launchID int64) {
	lastChange, err := db.LastProviderStatusTransitionTime(database, launchID)
	if err != nil {
		slog.Debug("last provider status change lookup failed", "component", "reconcile", "launch", launchID, "error", err)
		return
	}
	params.LastProviderStatusChangeAt = lastChange
}

// stalePreRunningAnchor returns the time from which rule 4b's
// "stale non-running status" timer should run: the later of the most
// recent provider status transition and the launch's lifecycle start.
// Falling back to lifecycle start preserves behaviour for launches
// that haven't yet recorded any transitions.
func stalePreRunningAnchor(ci *db.Launch, lastStatusChange *time.Time) *time.Time {
	lifecycle := cloudInstanceLifecycleStart(ci)
	switch {
	case lifecycle == nil:
		return lastStatusChange
	case lastStatusChange == nil:
		return lifecycle
	case lastStatusChange.After(*lifecycle):
		return lastStatusChange
	default:
		return lifecycle
	}
}

func cloudInstanceLifecycleStart(ci *db.Launch) *time.Time {
	if ci == nil {
		return nil
	}
	if ci.LaunchedAt != nil && *ci.LaunchedAt > 0 {
		ts := time.Unix(*ci.LaunchedAt, 0)
		return &ts
	}
	if ci.ReadyAt != nil && *ci.ReadyAt > 0 {
		ts := time.Unix(*ci.ReadyAt, 0)
		return &ts
	}
	if ci.CreatedAt > 0 {
		ts := time.Unix(ci.CreatedAt, 0)
		return &ts
	}
	return nil
}

func survivalDurations(s *db.BootstrapSurvival) db.BootstrapDurations {
	if s == nil {
		return nil
	}
	return s.Durations
}

func survivalTerminateAfter(s *db.BootstrapSurvival) time.Duration {
	if s == nil {
		return 0
	}
	return s.TerminateAfter
}

func clampPhaseChangedAtToInstanceLifecycle(changedAt *time.Time, ci *db.Launch) *time.Time {
	if changedAt == nil {
		return nil
	}
	lifecycleStart := cloudInstanceLifecycleStart(ci)
	if lifecycleStart == nil || !changedAt.Before(*lifecycleStart) {
		return changedAt
	}
	ts := *lifecycleStart
	return &ts
}

func inferInitialPhaseChangedAt(phase string, ci *db.Launch, jobs []*db.Job, timings map[int64]*db.JobPhaseTimings) *time.Time {
	verb, jobID, ok := ParsePhaseJobID(phase)
	if !ok {
		return nil
	}

	job := findJobInSlice(jobs, jobID)
	jobTimings := timings[jobID]

	switch verb {
	case PhaseSetup:
		if jobTimings != nil && jobTimings.SetupStart != nil && *jobTimings.SetupStart > 0 {
			ts := time.Unix(*jobTimings.SetupStart, 0)
			return clampPhaseChangedAtToInstanceLifecycle(&ts, ci)
		}
	case PhaseRunning:
		if job != nil && job.StartTime > 0 {
			ts := time.Unix(job.StartTime, 0)
			return clampPhaseChangedAtToInstanceLifecycle(&ts, ci)
		}
		if jobTimings != nil && jobTimings.RunStart != nil && *jobTimings.RunStart > 0 {
			ts := time.Unix(*jobTimings.RunStart, 0)
			return clampPhaseChangedAtToInstanceLifecycle(&ts, ci)
		}
	case PhaseFinalizing:
		if jobTimings != nil && jobTimings.RunEnd != nil && *jobTimings.RunEnd > 0 {
			ts := time.Unix(*jobTimings.RunEnd, 0)
			return clampPhaseChangedAtToInstanceLifecycle(&ts, ci)
		}
	case PhaseUploading:
		if jobTimings != nil && jobTimings.UploadStart != nil && *jobTimings.UploadStart > 0 {
			ts := time.Unix(*jobTimings.UploadStart, 0)
			return clampPhaseChangedAtToInstanceLifecycle(&ts, ci)
		}
	}
	return nil
}

// Phase verb constants for R2 instance phase strings (format "verb:jobID").
const (
	PhaseSetup            = "setup"
	PhaseGPUWarmup        = "gpu_warmup"
	PhaseRunning          = "running"
	PhaseFinalizing       = "finalizing"
	PhaseUploading        = "uploading"
	PhaseUploadingResults = "uploading-results"
	PhaseDiskFull         = "disk-full"
	PhaseGrace            = "grace"
	PhaseDestroying       = "destroying"
)

// findJobInSlice returns the first job in the slice matching the given ID, or nil.
func findJobInSlice(jobs []*db.Job, id int64) *db.Job {
	for _, j := range jobs {
		if j != nil && j.ID == id {
			return j
		}
	}
	return nil
}

func ParsePhaseJobID(phase string) (string, int64, bool) {
	verb, jobIDText, ok := strings.Cut(phase, ":")
	if !ok || jobIDText == "" {
		return "", 0, false
	}
	jobID, err := strconv.ParseInt(jobIDText, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return verb, jobID, true
}

func isActiveInstancePhase(phase string) bool {
	phase = strings.TrimSpace(phase)
	switch phase {
	case PhaseGrace, PhaseDestroying, PhaseDiskFull:
		return true
	}

	verb, _, ok := ParsePhaseJobID(phase)
	if !ok {
		return false
	}
	switch verb {
	case PhaseSetup, PhaseGPUWarmup, PhaseRunning, PhaseFinalizing, PhaseUploading, PhaseUploadingResults, PhaseDiskFull:
		return true
	default:
		return false
	}
}

// displayPhase reconciles raw R2 phase data with DB job state for display and checks.
// DB owns the coarse running state; R2 owns richer execution sub-states.
func displayPhase(jobStatuses map[int64]string, jobs []*db.Job, r2Phase string) (phase string, verb string) {
	r2Phase = strings.TrimSpace(r2Phase)
	switch r2Phase {
	case PhaseGrace, PhaseDestroying, PhaseDiskFull:
		return r2Phase, r2Phase
	}

	r2Verb, jobID, ok := ParsePhaseJobID(r2Phase)
	if !ok {
		if runningPhase, runningVerb := fallbackRunningPhase(jobStatuses, jobs); runningPhase != "" {
			return runningPhase, runningVerb
		}
		return r2Phase, ""
	}

	switch r2Verb {
	case PhaseSetup, PhaseGPUWarmup:
		// Once DB says the job is running, setup/warmup is stale metadata.
		if jobStatuses[jobID] == db.StatusRunning {
			return fmt.Sprintf("%s:%d", PhaseRunning, jobID), PhaseRunning
		}
	}
	return r2Phase, r2Verb
}

// DisplayPhase reconciles a stored or observed instance phase with current DB
// job state for read-only display paths.
func DisplayPhase(jobs []*db.Job, phase string) (string, string) {
	jobStatuses := make(map[int64]string, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		jobStatuses[job.ID] = job.Status
	}
	return displayPhase(jobStatuses, jobs, phase)
}

func fallbackRunningPhase(jobStatuses map[int64]string, jobs []*db.Job) (phase string, verb string) {
	var (
		bestJobID    int64
		bestStart    int64
		bestHasStart bool
	)
	for _, job := range jobs {
		if job == nil || jobStatuses[job.ID] != db.StatusRunning {
			continue
		}
		start := job.StartTime
		hasStart := start > 0
		if bestJobID == 0 {
			bestJobID = job.ID
			bestStart = start
			bestHasStart = hasStart
			continue
		}
		if hasStart && (!bestHasStart || start > bestStart) {
			bestJobID = job.ID
			bestStart = start
			bestHasStart = true
			continue
		}
		if hasStart == bestHasStart && start == bestStart && job.ID > bestJobID {
			bestJobID = job.ID
		}
	}
	if bestJobID == 0 {
		for jobID, status := range jobStatuses {
			if status == db.StatusRunning && jobID > bestJobID {
				bestJobID = jobID
			}
		}
	}
	if bestJobID == 0 {
		return "", ""
	}
	return fmt.Sprintf("%s:%d", PhaseRunning, bestJobID), PhaseRunning
}

// providerPollInterval grows the watch loop's provider poll delay while
// ShowInstance keeps returning not-found: doubling per consecutive
// not-found, capped at 8x the base interval. A gone instance is reaped by
// the heartbeat/bootstrap watchdogs, not by polling harder, and each
// vast.ai lookup shells out to a full account listing. Any other poll
// outcome resets to the base interval.
func providerPollInterval(base time.Duration, notFoundStreak int) time.Duration {
	if notFoundStreak <= 0 {
		return base
	}
	shift := notFoundStreak
	if shift > 3 {
		shift = 3
	}
	return base << shift
}

// WatchInstance polls DB and cloud provider, sends updates on the returned channel.
// Closes the channel when the instance reaches a terminal state or ctx is cancelled.
// r2Client may be nil, in which case bootstrap stage fetching is skipped.
func WatchInstance(ctx context.Context, client cloud.Client, database *sql.DB, cloudInstanceID int64, dbInterval, providerInterval time.Duration, r2Client ...*r2.Client) <-chan InstanceUpdate {
	ch := make(chan InstanceUpdate, 1)

	var r2c *r2.Client
	if len(r2Client) > 0 {
		r2c = r2Client[0]
	}

	go func() {
		defer close(ch)
		var lastProviderPoll time.Time
		var notFoundStreak int
		var cachedInstance *cloud.Instance
		// lastProviderErr persists the most recent poll's transient failure
		// across ticks (like cachedInstance), so CheckInstance keeps seeing
		// "status unknown" between polls instead of a spurious nil error.
		var lastProviderErr error
		var lastProviderStatus string
		var lastJobs []*db.Job
		watchReconciler := NewReconciler()
		var agentVersion string
		agentVersionFetched := false

		var survival *db.BootstrapSurvival
		survivalComputed := false

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			ci, err := db.GetLaunch(database, cloudInstanceID)
			if err != nil || ci == nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(dbInterval):
					continue
				}
			}

			// Fast path: if DB shows terminal, send final update and exit
			// immediately. Skip provider poll, R2 sync, and reconciliation —
			// they're unnecessary and can block for 30s+ each (vastai CLI
			// timeout, semaphore contention).
			if IsInstanceTerminal(ci.Status) {
				jobs, _ := db.GetLaunchJobsIncludingAttempts(database, cloudInstanceID)
				outcomes, _ := db.GetAttemptOutcomesByLaunch(database, cloudInstanceID)
				update := InstanceUpdate{
					Launch:             ci,
					Jobs:               jobs,
					JobAttemptOutcomes: outcomes,
					Instance:           cachedInstance,
				}
				select {
				case ch <- update:
				case <-ctx.Done():
				}
				return
			}

			if !survivalComputed {
				survivalComputed = true
				survival, _ = db.ComputeBootstrapSurvival(database, ci.Provider)
			}

			if fetched, err := db.GetLaunchJobsIncludingAttempts(database, cloudInstanceID); err == nil {
				lastJobs = fetched
			}
			jobs := lastJobs
			jobPhaseTimings := make(map[int64]*db.JobPhaseTimings, len(jobs))
			for _, j := range jobs {
				if timings, err := db.GetJobPhaseTimings(database, j.ID); err == nil && timings != nil {
					jobPhaseTimings[j.ID] = timings
				}
			}

			// Refresh cloud instance info periodically. Skip when no provider
			// client is available (e.g. config has no client for this
			// provider) — DB-only updates still flow.
			providerInstID := ci.EffectiveProviderID()
			if client != nil && providerInstID != "" && time.Since(lastProviderPoll) >= providerPollInterval(providerInterval, notFoundStreak) {
				inst, showErr := client.ShowInstance(providerInstID)
				if showErr == nil {
					cachedInstance = inst
					lastProviderErr = nil
					notFoundStreak = 0
					if inst.Status != lastProviderStatus {
						_ = db.RecordProviderStatus(database, cloudInstanceID, time.Now(), lastProviderStatus, inst.Status)
						lastProviderStatus = inst.Status
					}
				} else {
					if errors.Is(showErr, cloud.ErrInstanceNotFound) {
						// Confirmed absent at the provider — drop the stale
						// cached instance and keep the sentinel. Not-found is
						// non-authoritative for termination (vast.ai
						// transiently 404s still-booting instances), so rule 7
						// leaves adjudication to the heartbeat/bootstrap
						// watchdogs, matching the batch reconciler's not-found
						// handling.
						cachedInstance = nil
						notFoundStreak++
					}
					lastProviderErr = showErr
				}
				lastProviderPoll = time.Now()
			}

			attemptOutcomes, _ := db.GetAttemptOutcomesByLaunch(database, cloudInstanceID)
			jobState := ComputeJobState(jobs, attemptOutcomes)

			// Sync external state (R2 markers, termination intent) to launch_live_state.
			synced := SyncInstanceState(ctx, database, ci, r2c, jobs, jobState, SyncInstanceStateOpts{
				AgentVersion:        agentVersion,
				AgentVersionFetched: agentVersionFetched,
			})
			agentVersion = synced.AgentVersion
			agentVersionFetched = true

			// Run shared reconciliation checks
			now := time.Now()
			params := synced.CheckParams(ci, r2c, jobState, now)
			params.ProviderInst = cachedInstance
			params.ProviderErr = lastProviderErr
			params.ProviderStatusUnknownFor = watchReconciler.noteProviderStatusPoll(ci.ID, cachedInstance == nil && lastProviderErr != nil, now)
			params.PauseTolerant = hasPreemptibleJobs(jobs)
			params.BootstrapSurvival = survival
			resolveLastProviderStatusChange(database, &params, ci.ID)
			action := watchReconciler.CheckInstance(params)

			// Execute non-display actions (destroy, mark failed/completed, reset jobs)
			var stallMessage string
			if action.Kind != ActionNone && action.Kind != ActionDisplayOnly {
				slog.Info("watch action triggered", "component", "watch", "instance", cloudInstanceID, "action", action.Kind, "message", action.StallMessage)
				ExecuteAction(database, client, ci, action)
				// Refresh state after action
				ci, _ = db.GetLaunch(database, cloudInstanceID)
				if ci == nil {
					return
				}
				if fetched, err := db.GetLaunchJobsIncludingAttempts(database, cloudInstanceID); err == nil {
					lastJobs = fetched
				}
				jobs = lastJobs
				stallMessage = action.StallMessage
			} else if action.StallMessage != "" {
				stallMessage = action.StallMessage
			}

			// Refresh attempt outcomes so instance-scoped display tracks resets and
			// completions that may have happened during reconciliation.
			attemptOutcomes, _ = db.GetAttemptOutcomesByLaunch(database, cloudInstanceID)

			update := InstanceUpdate{
				Launch:                  ci,
				Jobs:                    jobs,
				JobPhaseTimings:         jobPhaseTimings,
				JobAttemptOutcomes:      attemptOutcomes,
				Instance:                cachedInstance,
				BootstrapStage:          synced.BootstrapStage,
				RawInstancePhase:        synced.RawInstancePhase,
				InstancePhase:           synced.InstancePhase,
				PhaseChangedAt:          synced.PhaseChangedAt,
				StallMessage:            stallMessage,
				JobProgress:             synced.JobProgress,
				JobProgressID:           synced.JobProgressID,
				JobProgressPhase:        synced.JobProgressPhase,
				HeartbeatAge:            synced.HeartbeatAge,
				Heartbeat:               synced.Heartbeat,
				TerminationIntent:       synced.TerminationIntent,
				BootstrapDurations:      survivalDurations(survival),
				BootstrapTerminateAfter: survivalTerminateAfter(survival),
			}

			select {
			case ch <- update:
			case <-ctx.Done():
				return
			}

			// Check for terminal state
			if IsInstanceTerminal(ci.Status) {
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(dbInterval):
			}
		}
	}()

	return ch
}

// fetchJobProgress extracts the running job ID from an instance phase string
// and fetches its progress from R2. The R2 value is either "pct" (single-phase)
// or "phase:pct" (multi-phase). Returns (0, -1, 0) if not applicable.
func fetchJobProgress(ctx context.Context, r2c *r2.Client, instancePhase string, jobs []*db.Job) (jobID int64, percent int, phase int) {
	verb, jid, ok := ParsePhaseJobID(instancePhase)
	if !ok || verb != PhaseRunning {
		return 0, -1, 0
	}
	pctStr, found, err := fetchR2Marker(ctx, r2c, jobAttemptProgressKey(jid, jobs))
	if err != nil || !found || pctStr == "" {
		return jid, -1, 0
	}
	// Parse "phase:pct" or "pct"
	if parts := strings.SplitN(pctStr, ":", 2); len(parts) == 2 {
		ph, err1 := strconv.Atoi(parts[0])
		pct, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil {
			return jid, pct, ph
		}
	}
	pct, err := strconv.Atoi(pctStr)
	if err != nil {
		return jid, -1, 0
	}
	return jid, pct, 0
}

func jobAttemptProgressKey(jobID int64, jobs []*db.Job) string {
	if job := findJobInSlice(jobs, jobID); job != nil && job.LatestRunID != nil {
		return r2keys.JobAttemptProgress(jobID, *job.LatestRunID)
	}
	return r2keys.JobAttemptProgress(jobID, 0)
}

// fetchR2Marker reads a string marker from R2 with a 3-second timeout.
func fetchR2Marker(ctx context.Context, r2Client *r2.Client, key string) (value string, found bool, err error) {
	if r2Client == nil {
		return "", false, nil
	}
	defer func() {
		if r := recover(); r != nil {
			value, found, err = "", false, fmt.Errorf("fetch R2 marker %s: panic: %v", key, r)
		}
	}()
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	data, err := r2Client.GetObject(ctx2, key)
	if err != nil {
		if r2.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(string(data)), true, nil
}

// fetchHeartbeat reads the heartbeat JSON from R2 and returns the parsed sample
// and time since last liveness signal. Returns (nil, 0) if no heartbeat is
// available. Liveness age is min(time-since-heartbeat, time-since-last-seen):
// the dedicated last-seen key is written by the sidecar before any sample
// collection, so it stays fresh even if nvidia-smi or another metric probe
// hangs.
func fetchHeartbeat(ctx context.Context, r2Client *r2.Client, instanceID int64) (*HeartbeatSample, time.Duration) {
	data, found, err := fetchR2Marker(ctx, r2Client, r2keys.InstanceHeartbeat(instanceID))
	if err != nil || !found || data == "" {
		return nil, 0
	}
	var sample HeartbeatSample
	if err := json.Unmarshal([]byte(data), &sample); err != nil {
		return nil, 0
	}
	age := time.Since(time.Unix(sample.Ts, 0))
	if lastSeen, found, err := fetchR2Marker(ctx, r2Client, r2keys.InstanceLastSeen(instanceID)); err == nil && found && lastSeen != "" {
		if ts, err := strconv.ParseInt(lastSeen, 10, 64); err == nil && ts > 0 {
			if alt := time.Since(time.Unix(ts, 0)); alt < age {
				age = alt
			}
		}
	}
	return &sample, age
}

// fetchBootstrapStage reads the bootstrap stage marker from R2 for an instance.
func fetchBootstrapStage(ctx context.Context, r2Client *r2.Client, instanceID int64) string {
	value, _, _ := fetchR2Marker(ctx, r2Client, r2keys.BootstrapStage(instanceID))
	return value
}

// fetchOnStartStage reads the OnStart shell's stage marker and its R2
// last-modified time. The marker is last-write-wins, so the modified time is
// when the stage last advanced; a frozen value means the OnStart chain stalled.
// Returns ("", nil) when the marker is absent or unreadable. The recover guard
// matches fetchR2Marker: stub R2 clients in tests panic rather than error.
func fetchOnStartStage(ctx context.Context, r2Client *r2.Client, instanceID int64) (stage string, changedAt *time.Time) {
	if r2Client == nil {
		return "", nil
	}
	defer func() {
		if recover() != nil {
			stage, changedAt = "", nil
		}
	}()
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	body, lastModified, err := r2Client.GetObjectWithMeta(ctx2, r2keys.InstanceOnStartStage(instanceID))
	if err != nil {
		return "", nil
	}
	stage = strings.TrimSpace(string(body))
	// The marker body is "<stage> <RFC3339 timestamp>"; keep the stage token.
	if i := strings.IndexByte(stage, ' '); i > 0 {
		stage = stage[:i]
	}
	if lastModified.IsZero() {
		return stage, nil
	}
	return stage, &lastModified
}

// fetchInstancePhase reads the instance phase marker from R2.
func fetchInstancePhase(ctx context.Context, r2Client *r2.Client, instanceID int64) string {
	value, _, _ := fetchR2Marker(ctx, r2Client, r2keys.InstancePhase(instanceID))
	return value
}

func failureTerminationReasonFromPhase(phase, fallback string) string {
	if strings.HasPrefix(phase, PhaseDiskFull+":") || phase == PhaseDiskFull {
		return db.TerminationReasonDiskFull
	}
	return fallback
}

func failureTerminationReasonFromR2(ctx context.Context, r2Client *r2.Client, instanceID int64, fallback string) string {
	if r2Client == nil {
		return fallback
	}
	if intent, err := fetchReconcileTerminationIntent(ctx, r2Client, instanceID); err == nil && intent != nil && intent.TerminationReason != "" {
		return intent.TerminationReason
	}
	phase := fetchInstancePhase(ctx, r2Client, instanceID)
	if reason := failureTerminationReasonFromPhase(phase, fallback); reason != fallback {
		return reason
	}
	if data, found, err := fetchR2Marker(ctx, r2Client, r2keys.InstanceDiskFailure(instanceID)); err == nil && found && data != "" {
		return db.TerminationReasonDiskFull
	}
	return fallback
}

// InstancePhaseLabel returns a human-readable label for an instance phase string.
func InstancePhaseLabel(phase string) string {
	switch phase {
	case PhaseGrace:
		return "grace period"
	case PhaseDestroying:
		return "self-destructing"
	case PhaseDiskFull:
		return "disk full"
	}
	if colon := strings.IndexByte(phase, ':'); colon >= 0 {
		verb := phase[:colon]
		jobID := phase[colon+1:]
		switch verb {
		case PhaseSetup:
			return fmt.Sprintf("setup (job %s)", jobID)
		case PhaseGPUWarmup:
			return fmt.Sprintf("gpu warmup (job %s)", jobID)
		case PhaseRunning:
			return fmt.Sprintf("running job %s", jobID)
		case PhaseFinalizing:
			return fmt.Sprintf("finalizing job %s", jobID)
		case PhaseUploading:
			return fmt.Sprintf("uploading outputs (job %s)", jobID)
		case PhaseUploadingResults:
			return fmt.Sprintf("uploading logs/results (job %s)", jobID)
		case PhaseDiskFull:
			return fmt.Sprintf("disk full (job %s)", jobID)
		}
	}
	return phase
}

func TerminationIntentLabel(marker *instanceintent.Marker) string {
	if !HasActiveTerminationIntent(marker) {
		return ""
	}
	reason := marker.TerminationReason
	if reason == "" {
		reason = marker.TerminalStatus
	}
	if reason == db.TerminationReasonCompleted || reason == db.LaunchStatusCompleted {
		reason = "after completion"
	}
	switch {
	case marker.LastError != "" && marker.DestroyAttempts > 0:
		return fmt.Sprintf("cleanup needs attention (%s)", reason)
	case marker.DestroyStartedAtUnix > 0:
		return fmt.Sprintf("cleanup in progress (%s)", reason)
	default:
		return fmt.Sprintf("cleanup requested (%s)", reason)
	}
}

func TerminationIntentDetail(marker *instanceintent.Marker) string {
	if !HasActiveTerminationIntent(marker) {
		return ""
	}
	parts := make([]string, 0, 4)
	switch {
	case marker.LastError != "" && marker.DestroyAttempts > 0:
		parts = append(parts, "destroy request failed; retry pending")
	case marker.DestroyStartedAtUnix > 0:
		parts = append(parts, "waiting for provider to confirm destruction")
	default:
		parts = append(parts, "waiting to start cleanup")
	}
	if marker.RequestedAtUnix > 0 {
		parts = append(parts, fmt.Sprintf("requested %s ago", time.Since(time.Unix(marker.RequestedAtUnix, 0)).Truncate(time.Second)))
	}
	if marker.DestroyAttempts > 1 || marker.LastError != "" {
		parts = append(parts, fmt.Sprintf("attempts=%d", marker.DestroyAttempts))
	}
	if marker.LastError != "" {
		parts = append(parts, "last error: "+truncateText(marker.LastError, 160))
	}
	return strings.Join(parts, " | ")
}

func HasActiveTerminationIntent(marker *instanceintent.Marker) bool {
	return marker != nil && marker.TerminalStatus != "" && marker.DestroySucceededAtUnix == 0
}

func truncateText(s string, maxLen int) string {
	return util.Truncate(s, maxLen)
}

// onStartFailureStages are the OnStart shell's terminal install-failure
// markers. They MUST stay in sync with the `*-failed` / `*-missing-exit`
// stages written by DefaultOnStartCmd in internal/cloud/types.go. Observing
// any of these as the current OnStart stage means a dependency install
// definitively failed, so the instance can never reach bootstrap.
var onStartFailureStages = map[string]bool{
	"apt-failed":          true,
	"uv-failed":           true,
	"rclone-failed":       true,
	"rclone-missing-exit": true,
}

// isOnStartFailureStage reports whether stage is a terminal OnStart failure
// marker (rule 4e).
func isOnStartFailureStage(stage string) bool {
	return onStartFailureStages[strings.TrimSpace(stage)]
}

// BootstrapStageLabel returns a human-readable label for a bootstrap stage.
func BootstrapStageLabel(stage string) string {
	if after, ok := strings.CutPrefix(stage, "failed:"); ok {
		return "bootstrap failed (" + after + ")"
	}
	if after, ok := strings.CutPrefix(stage, "downloading_hf:"); ok {
		return "downloading HF assets (" + after + ")"
	}
	if after, ok := strings.CutPrefix(stage, "downloading_models:"); ok {
		return "downloading models (" + after + ")"
	}
	if after, ok := strings.CutPrefix(stage, "sources_extracting:"); ok {
		return "extracting sources (" + after + ")"
	}
	switch stage {
	case "agent_installing":
		return "installing agent"
	case "agent_installed":
		return "agent installed"
	case "sources_extracting":
		return "extracting sources"
	case "sources_extracted":
		return "sources extracted"
	case "deps_installing":
		return "installing dependencies"
	case "deps_installed":
		return "dependencies installed"
	case "hf_tool_installing":
		return "installing HF download tool"
	case "hf_tool_installed":
		return "HF download tool installed"
	case bootstrapStageReady:
		return "ready"
	case "starting_jobs":
		return "starting jobs"
	case "agent_starting":
		return "starting agent"
	default:
		return stage
	}
}

// FormatPlainUpdate returns line-oriented output for an instance state change.
// Only returns lines for fields that changed between prev and curr.
// If prev is nil, all fields are reported.
func FormatPlainUpdate(prev, curr InstanceUpdate) string {
	var lines []string
	id := curr.Launch.ID
	instanceRef := ids.FormatInstanceID(id)

	// Report bootstrap stall warnings
	if curr.StallMessage != "" && curr.StallMessage != prev.StallMessage {
		lines = append(lines, fmt.Sprintf("instance %s: WARNING %s", instanceRef, curr.StallMessage))
	}

	// Report bootstrap stage changes
	if curr.BootstrapStage != "" && curr.BootstrapStage != prev.BootstrapStage {
		label := BootstrapStageLabel(curr.BootstrapStage)
		lines = append(lines, fmt.Sprintf("instance %s: bootstrap: %s", instanceRef, label))
	}

	// Report instance phase changes
	if curr.InstancePhase != "" && curr.InstancePhase != prev.InstancePhase {
		label := InstancePhaseLabel(curr.InstancePhase)
		line := fmt.Sprintf("instance %s: phase: %s", instanceRef, label)
		if curr.PhaseChangedAt != nil {
			line += fmt.Sprintf(" (for %s)", time.Since(*curr.PhaseChangedAt).Truncate(time.Second))
		}
		if curr.Heartbeat != nil {
			line += fmt.Sprintf("  (gpu %d°C %d%%, disk %s free)",
				curr.Heartbeat.GPUTempC, curr.Heartbeat.GPUUtilPct,
				formatBytes(curr.Heartbeat.DiskFreeBytes))
		}
		lines = append(lines, line)
	}

	if HasActiveTerminationIntent(curr.TerminationIntent) {
		prevLabel := TerminationIntentLabel(prev.TerminationIntent)
		currLabel := TerminationIntentLabel(curr.TerminationIntent)
		if currLabel != "" && currLabel != prevLabel {
			lines = append(lines, fmt.Sprintf("instance %s: %s", instanceRef, currLabel))
			if detail := TerminationIntentDetail(curr.TerminationIntent); detail != "" {
				lines = append(lines, fmt.Sprintf("instance %s: cleanup detail: %s", instanceRef, detail))
			}
		}
	}

	// Report job progress changes
	if curr.JobProgress >= 0 && (curr.JobProgress != prev.JobProgress || curr.JobProgressID != prev.JobProgressID || curr.JobProgressPhase != prev.JobProgressPhase) {
		if curr.JobProgressPhase > 1 {
			lines = append(lines, fmt.Sprintf("instance %s: job %s progress: phase %d %d%%", instanceRef, ids.FormatJobID(curr.JobProgressID), curr.JobProgressPhase, curr.JobProgress))
		} else {
			lines = append(lines, fmt.Sprintf("instance %s: job %s progress: %d%%", instanceRef, ids.FormatJobID(curr.JobProgressID), curr.JobProgress))
		}
	}

	if prev.Launch == nil || prev.Launch.Status != curr.Launch.Status {
		line := fmt.Sprintf("instance %s: status=%s", instanceRef, curr.Launch.Status)
		if reason := curr.Launch.TerminationReason; reason != "" && reason != db.TerminationReasonCompleted {
			line += fmt.Sprintf(" (%s)", reason)
		}
		providerInstID := curr.Launch.EffectiveProviderID()
		if providerInstID != "" {
			line += fmt.Sprintf(" provider_id=%s", providerInstID)
		}
		if curr.Instance != nil && curr.Instance.SSHHost != "" {
			line += fmt.Sprintf(" ssh=\"%s\"", FormatSSHCommand(curr.Instance))
		}
		lines = append(lines, line)

		// Show grace period help when entering grace status
		if curr.Launch.Status == db.LaunchStatusGrace {
			if label := curr.Launch.GraceStatusLabel(); label != "" {
				lines = append(lines, fmt.Sprintf("instance %s: %s", instanceRef, label))
			}
			// Show actionable commands for failed jobs
			for _, j := range curr.Jobs {
				if j.Status == db.StatusFailed {
					lines = append(lines, "")
					lines = append(lines, fmt.Sprintf("  To resubmit job %s with updated sources:", ids.FormatJobID(j.ID)))
					lines = append(lines, fmt.Sprintf("    weft instance submit %s %d", instanceRef, j.ID))
					lines = append(lines, "  To resubmit with a modified command:")
					lines = append(lines, fmt.Sprintf("    weft instance submit %s %d --command '...'", instanceRef, j.ID))
				}
			}
			lines = append(lines, "  To extend the grace period:")
			lines = append(lines, fmt.Sprintf("    weft instance extend %s 15m", instanceRef))
			lines = append(lines, "  To release the instance:")
			lines = append(lines, fmt.Sprintf("    weft instance release %s", instanceRef))
		}
	}

	// Report job status changes
	prevJobStatus := make(map[int64]string)
	for _, j := range prev.Jobs {
		prevJobStatus[j.ID] = AttemptDisplayStatus(j, prev.JobAttemptOutcomes)
	}
	for _, j := range curr.Jobs {
		displayStatus := AttemptDisplayStatus(j, curr.JobAttemptOutcomes)
		if prevJobStatus[j.ID] != displayStatus {
			line := fmt.Sprintf("instance %s: job %s status=%s dir=%s", instanceRef, ids.FormatJobID(j.ID), displayStatus, j.DirectoryTailDisplay())
			if j.ExitCode != nil {
				line += fmt.Sprintf(" exit=%d", *j.ExitCode)
			}
			lines = append(lines, line)
		}
	}

	return strings.Join(lines, "\n")
}

// IsJobTerminal returns true if a display status represents a terminal job state.
// This covers both job statuses (completed, failed) and attempt outcomes (orphaned, canceled).
func IsJobTerminal(displayStatus string) bool {
	switch displayStatus {
	case db.StatusCompleted, db.StatusFailed, db.StatusDead, db.StatusKilled,
		db.AttemptOutcomeOrphaned, db.AttemptOutcomeCancelled:
		return true
	}
	return false
}

// IsInstanceTerminal returns true if the instance status is a terminal state.
// Note: "grace" and "paused" are NOT terminal — the instance is still alive
// (grace: waiting for resubmission; paused: provider stopped, may resume).
// When you have a Launch struct, prefer inst.IsTerminal() instead.
func IsInstanceTerminal(status string) bool {
	return db.IsTerminalLaunchStatus(status)
}
