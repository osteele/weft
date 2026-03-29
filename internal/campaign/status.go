package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/r2keys"
)

// Bootstrap timeout thresholds.
const (
	bootstrapWarnTimeout      = 15 * time.Minute // warn after this long with no progress
	bootstrapTerminateTimeout = 20 * time.Minute // auto-terminate after this long
	bootstrapStageReady       = "ready"          // R2 marker value when bootstrap is complete
)

// Setup phase stall defaults (used when no survival data is available).
const (
	defaultSetupStallWarn      = 15 * time.Minute
	defaultSetupStallTerminate = 25 * time.Minute
)

// Heartbeat staleness threshold: warn if heartbeat is older than this.
const heartbeatStaleThreshold = 3 * time.Minute

var (
	fetchWatchBootstrapStage = fetchBootstrapStage
	fetchWatchHeartbeat      = fetchHeartbeat
	fetchWatchInstancePhase  = fetchInstancePhase
	fetchWatchJobProgress    = fetchJobProgress
)

// HeartbeatSample mirrors the agent's heartbeat JSON payload.
type HeartbeatSample struct {
	Ts             int64  `json:"ts"`
	Phase          string `json:"phase"`
	GPUUtilPct     int    `json:"gpu_util_pct"`
	GPUMemUsedMiB  int    `json:"gpu_mem_used_mib"`
	GPUMemTotalMiB int    `json:"gpu_mem_total_mib"`
	GPUTempC       int    `json:"gpu_temp_c"`
	HostRSSKB      int64  `json:"host_rss_kb"`
	HostMemTotalKB int64  `json:"host_mem_total_kb"`
	DiskFreeBytes  int64  `json:"disk_free_bytes"`
	DiskTotalBytes int64  `json:"disk_total_bytes"`
}

// InstanceUpdate is a snapshot of cloud instance + job state.
type InstanceUpdate struct {
	Launch             *db.Launch
	Jobs               []*db.Job
	JobPhaseTimings    map[int64]*db.JobPhaseTimings
	JobAttemptOutcomes map[int64]string // job_id → attempt outcome for this instance
	Instance           *cloud.Instance  // nil if not yet provisioned
	BootstrapStage     string           // current bootstrap stage from R2 (e.g. "agent_installed")
	InstancePhase      string           // current job execution phase from R2 (e.g. "running:123")
	PhaseChangedAt     *time.Time       // first observed time of the current phase within this watcher
	StallMessage       string           // non-empty if bootstrap appears stuck
	JobProgress        int              // -1 = no progress, 0-100 = raw percent within current phase
	JobProgressID      int64            // which job the progress is for
	JobProgressPhase   int              // 1-based phase number (0 = unknown/single-phase)
	HeartbeatAge       time.Duration    // time since last heartbeat (0 = no heartbeat fetched)
	Heartbeat          *HeartbeatSample // latest heartbeat metrics (nil if unavailable)
	TerminationIntent  *instanceintent.Marker
	BootstrapDurations db.BootstrapDurations // sorted historical bootstrap durations for conditional estimates
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
		var cachedInstance *cloud.Instance
		var lastProviderStatus string
		var currentPhase string
		var phaseChangedAt *time.Time
		watchReconciler := NewReconciler()
		// Staleness guard: track last-written values to skip no-op DB writes
		var lastWrittenPhase, lastWrittenBootstrap, lastWrittenHBJSON string
		var lastWrittenProgressPct int = -1
		var lastWrittenProgressID int64
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

			if !survivalComputed {
				survivalComputed = true
				survival, _ = db.ComputeBootstrapSurvival(database, ci.Provider)
			}

			jobs, _ := db.GetLaunchJobsIncludingAttempts(database, cloudInstanceID)
			jobPhaseTimings := make(map[int64]*db.JobPhaseTimings, len(jobs))
			for _, j := range jobs {
				if timings, err := db.GetJobPhaseTimings(database, j.ID); err == nil && timings != nil {
					jobPhaseTimings[j.ID] = timings
				}
			}

			// Refresh cloud instance info periodically
			providerInstID := ci.EffectiveProviderID()
			var providerErr error
			if providerInstID != "" && time.Since(lastProviderPoll) >= providerInterval {
				inst, showErr := client.ShowInstance(providerInstID)
				if showErr == nil {
					cachedInstance = inst
					if inst.Status != lastProviderStatus {
						if lastProviderStatus != "" {
							_ = db.InsertProviderStatusTransition(database, cloudInstanceID, time.Now(), lastProviderStatus, inst.Status)
						}
						lastProviderStatus = inst.Status
					}
				}
				providerErr = showErr
				lastProviderPoll = time.Now()
			}

			jobState := ComputeJobState(jobs)

			// Fetch bootstrap stage or instance phase from R2 and sync to DB
			var bootstrapStage, instancePhase string
			terminationIntent := ci.TerminationIntent
			jobProgress := -1
			var jobProgressID int64
			var jobProgressPhase int
			var heartbeatAge time.Duration
			var heartbeat *HeartbeatSample
			if r2c != nil && (ci.Status == db.LaunchStatusRunning || ci.Status == db.LaunchStatusGrace) {
				if fetchedIntent, err := fetchReconcileTerminationIntent(ctx, r2c, cloudInstanceID); err == nil && fetchedIntent != nil {
					terminationIntent = fetchedIntent
					_ = db.UpdateLaunchTerminationIntent(database, cloudInstanceID, fetchedIntent)
					ci.TerminationIntent = fetchedIntent
				}
				instancePhase = fetchWatchInstancePhase(ctx, r2c, cloudInstanceID)
				if instancePhase != "" {
					jobProgressID, jobProgress, jobProgressPhase = fetchWatchJobProgress(ctx, r2c, instancePhase, jobs)

					// Cloud jobs stay queued in the DB (unlike on-prem which uses sync to detect start).
					if verb, phaseJobID, ok := ParsePhaseJobID(instancePhase); ok && phaseJobID > 0 {
						switch verb {
						case PhaseRunning, PhaseUploading, PhaseUploadingResults, PhaseFinalizing:
							if j := findJobInSlice(jobs, phaseJobID); j != nil && j.Status == db.StatusQueued {
								if err := db.MarkQueuedJobRunning(database, phaseJobID); err != nil {
									slog.Warn("failed to mark job running from R2 phase", "component", "watch", "job_id", phaseJobID, "error", err)
								}
							}
						}
					}

					// Detect grace transition: R2 phase says "grace" but DB still says "running"
					if instancePhase == PhaseGrace && ci.Status == db.LaunchStatusRunning {
						if checkR2GraceStatus(r2c, ci, database) {
							ci.Status = db.LaunchStatusGrace
						}
					}
				} else if !jobState.HasStartedJob {
					bootstrapStage = fetchWatchBootstrapStage(ctx, r2c, cloudInstanceID)
				}

				// Fetch heartbeat from R2 (only when jobs have started)
				if jobState.HasStartedJob || instancePhase != "" {
					heartbeat, heartbeatAge = fetchWatchHeartbeat(ctx, r2c, cloudInstanceID)
				}

				// Sync completion for jobs the DB still thinks are running but
				// that aren't the current phase job (retried each poll until synced).
				if _, currentJobID, ok := ParsePhaseJobID(instancePhase); ok && currentJobID > 0 {
					for _, j := range jobs {
						if j.ID != currentJobID && j.Status == db.StatusRunning {
							if CheckAndSyncJobComplete(ctx, r2c, database, j.ID) {
								jobs, _ = db.GetLaunchJobsIncludingAttempts(database, cloudInstanceID)
								jobState = ComputeJobState(jobs)
								break // re-evaluate on next poll with fresh job list
							}
						}
					}
				}

				// Fetch agent version once (static per instance)
				if !agentVersionFetched {
					agentVersionFetched = true
					agentVersion = fetchR2Marker(ctx, r2c, r2keys.InstanceAgentVersion(cloudInstanceID))
				}

				// Write live state to DB (staleness guard: skip if unchanged)
				var hbJSON string
				var hbTS int64
				if heartbeat != nil {
					if data, err := json.Marshal(heartbeat); err == nil {
						hbJSON = string(data)
					}
					hbTS = heartbeat.Ts
				}
				if instancePhase != lastWrittenPhase || bootstrapStage != lastWrittenBootstrap ||
					hbJSON != lastWrittenHBJSON || jobProgress != lastWrittenProgressPct ||
					jobProgressID != lastWrittenProgressID {
					_ = db.UpsertLaunchLiveState(database, db.LaunchLiveState{
						LaunchID:       cloudInstanceID,
						InstancePhase:  instancePhase,
						BootstrapStage: bootstrapStage,
						HeartbeatJSON:  hbJSON,
						HeartbeatTS:    hbTS,
						JobProgressPct: jobProgress,
						JobProgressID:  jobProgressID,
						AgentVersion:   agentVersion,
					})
					lastWrittenPhase = instancePhase
					lastWrittenBootstrap = bootstrapStage
					lastWrittenHBJSON = hbJSON
					lastWrittenProgressPct = jobProgress
					lastWrittenProgressID = jobProgressID
				}
			}

			// Run shared reconciliation checks
			now := time.Now()
			action := watchReconciler.CheckInstance(CheckInstanceParams{
				CI:                ci,
				ProviderInst:      cachedInstance,
				ProviderErr:       providerErr,
				R2Client:          r2c,
				JobState:          jobState,
				InstancePhase:     instancePhase,
				BootstrapStage:    bootstrapStage,
				HeartbeatAge:      heartbeatAge,
				Now:               now,
				TerminationIntent: terminationIntent,
				BootstrapSurvival: survival,
				PhaseChangedAt:    phaseChangedAt,
			})

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
				jobs, _ = db.GetLaunchJobsIncludingAttempts(database, cloudInstanceID)
				stallMessage = action.StallMessage
			} else if action.StallMessage != "" {
				stallMessage = action.StallMessage
			}

			if instancePhase == "" {
				currentPhase = ""
				phaseChangedAt = nil
			} else if instancePhase != currentPhase {
				prevPhase := currentPhase
				currentPhase = instancePhase
				if prevPhase == "" {
					phaseChangedAt = inferInitialPhaseChangedAt(instancePhase, ci, jobs, jobPhaseTimings)
				} else {
					phaseChangedAt = &now
				}
			}

			// Fetch attempt outcomes only for terminal instances (outcomes are immutable)
			var attemptOutcomes map[int64]string
			if IsInstanceTerminal(ci.Status) {
				attemptOutcomes, _ = db.GetAttemptOutcomesByLaunch(database, cloudInstanceID)
			}

			update := InstanceUpdate{
				Launch:             ci,
				Jobs:               jobs,
				JobPhaseTimings:    jobPhaseTimings,
				JobAttemptOutcomes: attemptOutcomes,
				Instance:           cachedInstance,
				BootstrapStage:     bootstrapStage,
				InstancePhase:      instancePhase,
				PhaseChangedAt:     phaseChangedAt,
				StallMessage:       stallMessage,
				JobProgress:        jobProgress,
				JobProgressID:      jobProgressID,
				JobProgressPhase:   jobProgressPhase,
				HeartbeatAge:       heartbeatAge,
				Heartbeat:          heartbeat,
				TerminationIntent:  terminationIntent,
				BootstrapDurations: survivalDurations(survival),
			}

			select {
			case ch <- update:
			case <-ctx.Done():
				return
			}

			// Check for terminal state
			if IsInstanceTerminal(ci.Status) {
				_ = db.DeleteLaunchLiveState(database, cloudInstanceID)
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
	pctStr := fetchR2Marker(ctx, r2c, jobAttemptProgressKey(jid, jobs))
	if pctStr == "" {
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
func fetchR2Marker(ctx context.Context, r2Client *r2.Client, key string) (value string) {
	if r2Client == nil {
		return ""
	}
	defer func() {
		if recover() != nil {
			value = ""
		}
	}()
	ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	data, err := r2Client.GetObject(ctx2, key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// fetchHeartbeat reads the heartbeat JSON from R2 and returns the parsed sample
// and time since last heartbeat. Returns (nil, 0) if no heartbeat is available.
func fetchHeartbeat(ctx context.Context, r2Client *r2.Client, instanceID int64) (*HeartbeatSample, time.Duration) {
	data := fetchR2Marker(ctx, r2Client, r2keys.InstanceHeartbeat(instanceID))
	if data == "" {
		return nil, 0
	}
	var sample HeartbeatSample
	if err := json.Unmarshal([]byte(data), &sample); err != nil {
		return nil, 0
	}
	age := time.Since(time.Unix(sample.Ts, 0))
	return &sample, age
}

// fetchBootstrapStage reads the bootstrap stage marker from R2 for an instance.
func fetchBootstrapStage(ctx context.Context, r2Client *r2.Client, instanceID int64) string {
	return fetchR2Marker(ctx, r2Client, r2keys.BootstrapStage(instanceID))
}

// fetchInstancePhase reads the instance phase marker from R2.
func fetchInstancePhase(ctx context.Context, r2Client *r2.Client, instanceID int64) string {
	return fetchR2Marker(ctx, r2Client, r2keys.InstancePhase(instanceID))
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
	if data := fetchR2Marker(ctx, r2Client, r2keys.InstanceDiskFailure(instanceID)); data != "" {
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
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

// BootstrapStageLabel returns a human-readable label for a bootstrap stage.
func BootstrapStageLabel(stage string) string {
	if after, ok := strings.CutPrefix(stage, "downloading_models:"); ok {
		return "downloading models (" + after + ")"
	}
	switch stage {
	case "agent_installed":
		return "installing agent"
	case "sources_extracted":
		return "extracting sources"
	case "deps_installed":
		return "installing dependencies"
	case bootstrapStageReady:
		return "ready"
	case "starting_jobs":
		return "starting jobs"
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

	// Report bootstrap stall warnings
	if curr.StallMessage != "" && curr.StallMessage != prev.StallMessage {
		lines = append(lines, fmt.Sprintf("instance %d: WARNING %s", id, curr.StallMessage))
	}

	// Report bootstrap stage changes
	if curr.BootstrapStage != "" && curr.BootstrapStage != prev.BootstrapStage {
		label := BootstrapStageLabel(curr.BootstrapStage)
		lines = append(lines, fmt.Sprintf("instance %d: bootstrap: %s", id, label))
	}

	// Report instance phase changes
	if curr.InstancePhase != "" && curr.InstancePhase != prev.InstancePhase {
		label := InstancePhaseLabel(curr.InstancePhase)
		line := fmt.Sprintf("instance %d: phase: %s", id, label)
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
			lines = append(lines, fmt.Sprintf("instance %d: %s", id, currLabel))
			if detail := TerminationIntentDetail(curr.TerminationIntent); detail != "" {
				lines = append(lines, fmt.Sprintf("instance %d: cleanup detail: %s", id, detail))
			}
		}
	}

	// Report job progress changes
	if curr.JobProgress >= 0 && (curr.JobProgress != prev.JobProgress || curr.JobProgressID != prev.JobProgressID || curr.JobProgressPhase != prev.JobProgressPhase) {
		if curr.JobProgressPhase > 1 {
			lines = append(lines, fmt.Sprintf("instance %d: job %d progress: phase %d %d%%", id, curr.JobProgressID, curr.JobProgressPhase, curr.JobProgress))
		} else {
			lines = append(lines, fmt.Sprintf("instance %d: job %d progress: %d%%", id, curr.JobProgressID, curr.JobProgress))
		}
	}

	if prev.Launch == nil || prev.Launch.Status != curr.Launch.Status {
		line := fmt.Sprintf("instance %d: status=%s", id, curr.Launch.Status)
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
				lines = append(lines, fmt.Sprintf("instance %d: %s", id, label))
			}
			// Show actionable commands for failed jobs
			for _, j := range curr.Jobs {
				if j.Status == db.StatusFailed {
					lines = append(lines, "")
					lines = append(lines, fmt.Sprintf("  To resubmit job %d with updated sources:", j.ID))
					lines = append(lines, fmt.Sprintf("    weft instance submit %d %d", id, j.ID))
					lines = append(lines, "  To resubmit with a modified command:")
					lines = append(lines, fmt.Sprintf("    weft instance submit %d %d --command '...'", id, j.ID))
				}
			}
			lines = append(lines, "  To extend the grace period:")
			lines = append(lines, fmt.Sprintf("    weft instance extend %d 15m", id))
			lines = append(lines, "  To release the instance:")
			lines = append(lines, fmt.Sprintf("    weft instance release %d", id))
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
			line := fmt.Sprintf("instance %d: job %d status=%s dir=%s", id, j.ID, displayStatus, j.DirectoryTailDisplay())
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
	case db.StatusCompleted, db.StatusFailed,
		db.AttemptOutcomeOrphaned, db.AttemptOutcomeCancelled:
		return true
	}
	return false
}

// IsInstanceTerminal returns true if the instance status is a terminal state.
// Note: "grace" is NOT terminal — the instance is still alive waiting for resubmission.
// When you have a Launch struct, prefer inst.IsTerminal() instead.
func IsInstanceTerminal(status string) bool {
	return status == db.LaunchStatusCompleted ||
		status == db.LaunchStatusFailed ||
		status == db.LaunchStatusCancelled
}
