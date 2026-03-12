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
	JobAttemptOutcomes map[int64]string // job_id → attempt outcome for this instance
	Instance           *cloud.Instance  // nil if not yet provisioned
	BootstrapStage     string           // current bootstrap stage from R2 (e.g. "agent_installed")
	InstancePhase      string           // current job execution phase from R2 (e.g. "running:123")
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
		var firstDeadAt time.Time

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

			// Refresh cloud instance info periodically
			providerInstID := ci.EffectiveProviderID()
			if providerInstID != "" && time.Since(lastProviderPoll) >= providerInterval {
				inst, showErr := client.ShowInstance(providerInstID)
				if showErr == nil {
					cachedInstance = inst
				}
				lastProviderPoll = time.Now()

				// Detect dead instances: provider says dead but DB says running.
				// Skip grace-period instances — they legitimately keep the provider
				// alive, and a transient API failure shouldn't kill the grace session.
				if showErr == nil && isProviderTerminal(inst) && !IsInstanceTerminal(ci.Status) && ci.Status != db.CloudInstanceStatusGrace {
					if firstDeadAt.IsZero() {
						firstDeadAt = time.Now()
						status := "not found"
						if inst != nil {
							status = inst.Status
						}
						log.Printf("watch: cloud instance %d appears dead (status: %s), waiting %s to confirm",
							cloudInstanceID, status, minDeadConfirmTime)
					}
					if time.Since(firstDeadAt) >= minDeadConfirmTime {
						if r2c != nil && hasR2CompletionMarker(r2c, cloudInstanceID) {
							_ = db.UpdateCloudInstanceStatus(database, cloudInstanceID, db.CloudInstanceStatusCompleted, db.TerminationReasonCompleted)
							_ = db.CloseJobCloudAttemptsByInstance(database, cloudInstanceID, db.AttemptOutcomeCompleted)
							ci.Status = db.CloudInstanceStatusCompleted
							jobs, _ = db.GetCloudInstanceJobsIncludingAttempts(database, cloudInstanceID)
						} else {
							reason := failureTerminationReasonFromR2(ctx, r2c, cloudInstanceID, db.TerminationReasonPreempted)
							_ = db.UpdateCloudInstanceStatus(database, cloudInstanceID, db.CloudInstanceStatusFailed, reason)
							_, _ = db.ResetCloudInstanceJobs(database, cloudInstanceID, db.AttemptOutcomeOrphaned)
							ci.Status = db.CloudInstanceStatusFailed
							ci.TerminationReason = reason
							jobs, _ = db.GetCloudInstanceJobsIncludingAttempts(database, cloudInstanceID)
						}
						firstDeadAt = time.Time{}
					}
				} else if showErr == nil && !isProviderTerminal(inst) {
					firstDeadAt = time.Time{}
				}
			}

			// Check if any job has progressed beyond queued
			hasStartedJob := false
			for _, j := range jobs {
				if j.Status != db.StatusQueued {
					hasStartedJob = true
					break
				}
			}

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
				if hasStartedJob {
					instancePhase = fetchInstancePhase(ctx, r2c, cloudInstanceID)
					jobProgressID, jobProgress = fetchJobProgress(ctx, r2c, instancePhase)

					// Detect grace transition: R2 phase says "grace" but DB still says "running"
					if instancePhase == "grace" && ci.Status == db.CloudInstanceStatusRunning {
						if checkR2GraceStatus(r2c, ci, database) {
							ci.Status = db.CloudInstanceStatusGrace
						}
					}
				} else {
					bootstrapStage = fetchBootstrapStage(ctx, r2c, cloudInstanceID)
				}
			}

			// Fetch heartbeat from R2 (only when jobs have started)
			var heartbeatAge time.Duration
			var heartbeat *HeartbeatSample
			if r2c != nil && hasStartedJob && (ci.Status == db.CloudInstanceStatusRunning || ci.Status == db.CloudInstanceStatusGrace) {
				heartbeat, heartbeatAge = fetchHeartbeat(ctx, r2c, cloudInstanceID)
			}

			// Detect bootstrap stall: instance running but no job progress
			var stallMessage string
			if ci.Status == db.CloudInstanceStatusRunning && ci.LaunchedAt != nil && !hasStartedJob && bootstrapStage != bootstrapStageReady {
				// Check R2 for completion marker before declaring a stall — the agent
				// may have completed all jobs but failed to self-destruct, so the local
				// DB still shows jobs as queued while the instance is actually done.
				instanceComplete := hasR2CompletionMarker(r2c, cloudInstanceID)

				elapsed := time.Since(time.Unix(*ci.LaunchedAt, 0))
				if instanceComplete {
					// Instance completed its work but self-destruct failed.
					// Clean up the provider instance and mark as completed, not failed.
					if providerInstID != "" {
						_ = client.DestroyInstance(providerInstID)
					}
					_ = db.UpdateCloudInstanceStatus(database, cloudInstanceID, db.CloudInstanceStatusCompleted)
					ci.Status = db.CloudInstanceStatusCompleted
					jobs, _ = db.GetCloudInstanceJobsIncludingAttempts(database, cloudInstanceID)
					stallMessage = "instance completed but self-destruct failed — cleaning up"
				} else if elapsed >= bootstrapTerminateTimeout {
					// Auto-terminate: destroy provider instance, mark failed, reset jobs
					if providerInstID != "" {
						_ = client.DestroyInstance(providerInstID)
					}
					_ = db.UpdateCloudInstanceStatus(database, cloudInstanceID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure)
					_, _ = db.ResetCloudInstanceJobs(database, cloudInstanceID, db.AttemptOutcomeOrphaned)
					ci.Status = db.CloudInstanceStatusFailed
					jobs, _ = db.GetCloudInstanceJobsIncludingAttempts(database, cloudInstanceID)
					stallMessage = fmt.Sprintf("bootstrap timeout after %s — terminating instance, jobs reset to queued",
						elapsed.Truncate(time.Minute))
				} else if elapsed >= bootstrapWarnTimeout {
					stallMessage = fmt.Sprintf("bootstrap stalled — no activity after %s",
						elapsed.Truncate(time.Minute))
				}
			}

			// Detect stale heartbeat: agent may have crashed
			if heartbeatAge > heartbeatStaleThreshold && stallMessage == "" {
				stallMessage = fmt.Sprintf("heartbeat stale (%s since last update)", heartbeatAge.Truncate(time.Second))
			}

			// Detect failed self-destruct: all jobs finished but instance still running.
			// Give 2 minutes after the last job for uploads + self-destruct attempts.
			if ci.Status == db.CloudInstanceStatusRunning && hasStartedJob && len(jobs) > 0 {
				allJobsTerminal := true
				var latestEnd int64
				for _, j := range jobs {
					if !IsJobTerminal(j.Status) {
						allJobsTerminal = false
						break
					}
					if j.EndTime != nil && *j.EndTime > latestEnd {
						latestEnd = *j.EndTime
					}
				}
				if allJobsTerminal && latestEnd > 0 && time.Since(time.Unix(latestEnd, 0)) > 2*time.Minute {
					// Self-destruct failed — clean up
					if providerInstID != "" {
						_ = client.DestroyInstance(providerInstID)
					}
					_ = db.UpdateCloudInstanceStatus(database, cloudInstanceID, db.CloudInstanceStatusCompleted)
					ci.Status = db.CloudInstanceStatusCompleted
					stallMessage = "all jobs finished but self-destruct failed — cleaning up"
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
				JobAttemptOutcomes: attemptOutcomes,
				Instance:           cachedInstance,
				BootstrapStage:     bootstrapStage,
				InstancePhase:      instancePhase,
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
func fetchJobProgress(ctx context.Context, r2c *r2.Client, instancePhase string) (jobID int64, percent int) {
	verb, idStr, ok := strings.Cut(instancePhase, ":")
	if !ok || verb != "running" {
		return 0, -1
	}
	jid, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, -1
	}
	pctStr := fetchR2Marker(ctx, r2c, r2keys.JobProgress(jid))
	if pctStr == "" {
		return jid, -1
	}
	pct, err := strconv.Atoi(pctStr)
	if err != nil {
		return jid, -1
	}
	return jid, pct
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
	if strings.HasPrefix(phase, "disk-full:") || phase == "disk-full" {
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
	case "grace":
		return "grace period"
	case "destroying":
		return "self-destructing"
	case "disk-full":
		return "disk full"
	}
	if colon := strings.IndexByte(phase, ':'); colon >= 0 {
		verb := phase[:colon]
		jobID := phase[colon+1:]
		switch verb {
		case "setup":
			return fmt.Sprintf("setup (job %s)", jobID)
		case "running":
			return fmt.Sprintf("running job %s", jobID)
		case "uploading":
			return fmt.Sprintf("uploading outputs (job %s)", jobID)
		case "disk-full":
			return fmt.Sprintf("disk full (job %s)", jobID)
		}
	}
	return phase
}

func TerminationIntentLabel(marker *instanceintent.Marker) string {
	if marker == nil {
		return ""
	}
	reason := marker.TerminationReason
	if reason == "" {
		reason = marker.TerminalStatus
	}
	switch {
	case marker.DestroySucceededAtUnix > 0:
		return fmt.Sprintf("self-destruct succeeded (%s)", reason)
	case marker.LastError != "" && marker.DestroyAttempts > 0:
		return fmt.Sprintf("self-destruct retry %d failed (%s)", marker.DestroyAttempts, reason)
	case marker.DestroyStartedAtUnix > 0:
		if marker.DestroyAttempts > 0 {
			return fmt.Sprintf("self-destructing (%s, attempt %d)", reason, marker.DestroyAttempts)
		}
		return fmt.Sprintf("self-destructing (%s)", reason)
	default:
		return fmt.Sprintf("termination requested (%s)", reason)
	}
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
		if curr.Heartbeat != nil {
			line += fmt.Sprintf("  (gpu %d°C %d%%, disk %s free)",
				curr.Heartbeat.GPUTempC, curr.Heartbeat.GPUUtilPct,
				formatBytes(curr.Heartbeat.DiskFreeBytes))
		}
		lines = append(lines, line)
	}

	if curr.TerminationIntent != nil {
		prevLabel := TerminationIntentLabel(prev.TerminationIntent)
		currLabel := TerminationIntentLabel(curr.TerminationIntent)
		if currLabel != "" && currLabel != prevLabel {
			lines = append(lines, fmt.Sprintf("instance %d: %s", id, currLabel))
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
// This covers both job statuses (completed, failed) and attempt outcomes (orphaned, cancelled).
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
