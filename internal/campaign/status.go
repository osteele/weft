package campaign

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
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
	CloudInstance      *db.CloudInstance
	Jobs               []*db.Job
	JobPhaseTimings    map[int64]*db.JobPhaseTimings
	JobAttemptOutcomes map[int64]string // job_id → attempt outcome for this instance
	Instance           *cloud.Instance  // nil if not yet provisioned
	BootstrapStage     string           // current bootstrap stage from R2 (e.g. "agent_installed")
	InstancePhase      string           // current job execution phase from R2 (e.g. "running:123")
	PhaseChangedAt     *time.Time       // first observed time of the current phase within this watcher
	StallMessage       string           // non-empty if bootstrap appears stuck
	JobProgress        int              // -1 = no progress, 0-100 = percent
	JobProgressID      int64            // which job the progress is for
	HeartbeatAge       time.Duration    // time since last heartbeat (0 = no heartbeat fetched)
	Heartbeat          *HeartbeatSample // latest heartbeat metrics (nil if unavailable)
	TerminationIntent  *instanceintent.Marker
}

// JobDisplayStatus returns the status to display for a job in the context of a
// specific instance. When a job has been reset to "queued" after an instance
// failure, this returns the attempt outcome (e.g. "failed") instead.
func JobDisplayStatus(j *db.Job, outcomes map[int64]string) string {
	if outcome, ok := outcomes[j.ID]; ok && j.Status == db.StatusQueued {
		return outcome
	}
	return j.Status
}

func cloudInstanceLifecycleStart(ci *db.CloudInstance) *time.Time {
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

func clampPhaseChangedAtToInstanceLifecycle(changedAt *time.Time, ci *db.CloudInstance) *time.Time {
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

func inferInitialPhaseChangedAt(phase string, ci *db.CloudInstance, jobs []*db.Job, timings map[int64]*db.JobPhaseTimings) *time.Time {
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

	// Extract optional r2Client
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

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			ci, err := db.GetCloudInstance(database, cloudInstanceID)
			if err != nil || ci == nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(dbInterval):
					continue
				}
			}

			jobs, _ := db.GetCloudInstanceJobsIncludingAttempts(database, cloudInstanceID)
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

			// Fetch bootstrap stage or instance phase from R2
			var bootstrapStage, instancePhase string
			terminationIntent := ci.TerminationIntent
			jobProgress := -1
			var jobProgressID int64
			if r2c != nil && (ci.Status == db.CloudInstanceStatusRunning || ci.Status == db.CloudInstanceStatusGrace) {
				if fetchedIntent, err := fetchReconcileTerminationIntent(ctx, r2c, cloudInstanceID); err == nil && fetchedIntent != nil {
					terminationIntent = fetchedIntent
					_ = db.UpdateCloudInstanceTerminationIntent(database, cloudInstanceID, fetchedIntent)
					ci.TerminationIntent = fetchedIntent
				}
				instancePhase = fetchWatchInstancePhase(ctx, r2c, cloudInstanceID)
				if instancePhase != "" {
					jobProgressID, jobProgress = fetchWatchJobProgress(ctx, r2c, instancePhase, jobs)

					// Cloud jobs stay queued in the DB (unlike on-prem which uses sync to detect start).
					if verb, phaseJobID, ok := ParsePhaseJobID(instancePhase); ok && phaseJobID > 0 {
						switch verb {
						case PhaseRunning, PhaseUploading, PhaseUploadingResults, PhaseFinalizing:
							if j := findJobInSlice(jobs, phaseJobID); j != nil && j.Status == db.StatusQueued {
								if err := db.MarkQueuedJobRunning(database, phaseJobID); err != nil {
									log.Printf("watch: mark job %d running from R2 phase: %v", phaseJobID, err)
								}
							}
						}
					}

					// Detect grace transition: R2 phase says "grace" but DB still says "running"
					if instancePhase == PhaseGrace && ci.Status == db.CloudInstanceStatusRunning {
						if checkR2GraceStatus(r2c, ci, database) {
							ci.Status = db.CloudInstanceStatusGrace
						}
					}
				} else if !jobState.HasStartedJob {
					bootstrapStage = fetchWatchBootstrapStage(ctx, r2c, cloudInstanceID)
				}
			}

			// Fetch heartbeat from R2 (only when jobs have started)
			var heartbeatAge time.Duration
			var heartbeat *HeartbeatSample
			if r2c != nil && (jobState.HasStartedJob || instancePhase != "") && (ci.Status == db.CloudInstanceStatusRunning || ci.Status == db.CloudInstanceStatusGrace) {
				heartbeat, heartbeatAge = fetchWatchHeartbeat(ctx, r2c, cloudInstanceID)
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
			})

			// Execute non-display actions (destroy, mark failed/completed, reset jobs)
			var stallMessage string
			if action.Kind != ActionNone && action.Kind != ActionDisplayOnly {
				log.Printf("watch: instance %d action=%d (%s)", cloudInstanceID, action.Kind, action.StallMessage)
				ExecuteAction(database, client, ci, action)
				// Refresh state after action
				ci, _ = db.GetCloudInstance(database, cloudInstanceID)
				if ci == nil {
					return
				}
				jobs, _ = db.GetCloudInstanceJobsIncludingAttempts(database, cloudInstanceID)
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
				attemptOutcomes, _ = db.GetAttemptOutcomesByInstance(database, cloudInstanceID)
			}

			update := InstanceUpdate{
				CloudInstance:      ci,
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
				HeartbeatAge:       heartbeatAge,
				Heartbeat:          heartbeat,
				TerminationIntent:  terminationIntent,
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
// and fetches its progress percentage from R2. Returns (0, -1) if not applicable.
func fetchJobProgress(ctx context.Context, r2c *r2.Client, instancePhase string, jobs []*db.Job) (jobID int64, percent int) {
	verb, jid, ok := ParsePhaseJobID(instancePhase)
	if !ok || verb != PhaseRunning {
		return 0, -1
	}
	pctStr := fetchR2Marker(ctx, r2c, jobAttemptProgressKey(jid, jobs))
	if pctStr == "" {
		return jid, -1
	}
	pct, err := strconv.Atoi(pctStr)
	if err != nil {
		return jid, -1
	}
	return jid, pct
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
	if reason == db.TerminationReasonCompleted || reason == db.CloudInstanceStatusCompleted {
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
	id := curr.CloudInstance.ID

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
	if curr.JobProgress >= 0 && (curr.JobProgress != prev.JobProgress || curr.JobProgressID != prev.JobProgressID) {
		lines = append(lines, fmt.Sprintf("instance %d: job %d progress: %d%%", id, curr.JobProgressID, curr.JobProgress))
	}

	if prev.CloudInstance == nil || prev.CloudInstance.Status != curr.CloudInstance.Status {
		line := fmt.Sprintf("instance %d: status=%s", id, curr.CloudInstance.Status)
		if reason := curr.CloudInstance.TerminationReason; reason != "" && reason != db.TerminationReasonCompleted {
			line += fmt.Sprintf(" (%s)", reason)
		}
		providerInstID := curr.CloudInstance.EffectiveProviderID()
		if providerInstID != "" {
			line += fmt.Sprintf(" provider_id=%s", providerInstID)
		}
		if curr.Instance != nil && curr.Instance.SSHHost != "" {
			line += fmt.Sprintf(" ssh=\"%s\"", FormatSSHCommand(curr.Instance))
		}
		lines = append(lines, line)

		// Show grace period help when entering grace status
		if curr.CloudInstance.Status == db.CloudInstanceStatusGrace {
			if label := curr.CloudInstance.GraceStatusLabel(); label != "" {
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
		prevJobStatus[j.ID] = JobDisplayStatus(j, prev.JobAttemptOutcomes)
	}
	for _, j := range curr.Jobs {
		displayStatus := JobDisplayStatus(j, curr.JobAttemptOutcomes)
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
// When you have a CloudInstance struct, prefer inst.IsTerminal() instead.
func IsInstanceTerminal(status string) bool {
	return status == db.CloudInstanceStatusCompleted ||
		status == db.CloudInstanceStatusFailed ||
		status == db.CloudInstanceStatusCancelled
}
