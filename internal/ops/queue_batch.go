package ops

import (
	"database/sql"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ssh"
)

type queueBatchStatus struct {
	State         queueState
	ExitCode      *int
	Mtime         int64
	RunID         int64
	GPUDevices    string
	FailureReason string
	AgentVersion  string
	Source        *db.JobSourceExecutionMetadata
	FromR2        bool
}

var syncJobStatusFromR2ForBatch = syncJobStatusFromR2

// BatchSyncQueueRunnerJobs performs a batched sync for queue-runner jobs on one host/queue.
// This avoids per-job SSH calls by fetching queue state in a single command.
func BatchSyncQueueRunnerJobs(database *sql.DB, host string, jobs []*db.Job, timeout time.Duration) (int, error) {
	if len(jobs) == 0 {
		return 0, nil
	}
	jobIDs := make([]int64, 0, len(jobs))
	jobByID := make(map[int64]*db.Job, len(jobs))
	for _, job := range jobs {
		if job == nil {
			continue
		}
		jobIDs = append(jobIDs, job.ID)
		jobByID[job.ID] = job
	}
	if len(jobIDs) == 0 {
		return 0, nil
	}

	statuses, err := fetchQueueBatchStatus(host, jobIDs, timeout)
	if err != nil {
		if hostUsesR2Queue(host) {
			updated := syncMissingR2Completions(database, jobs, nil)
			updated += syncInventoryPublicationReports(database, jobs)
			if updated > 0 {
				return updated, nil
			}
		}
		return 0, err
	}

	updated, err := applyBatchStatuses(database, jobIDs, jobByID, statuses, timeout)
	if err != nil {
		return updated, err
	}
	if hostUsesR2Queue(host) {
		updated += syncMissingR2Completions(database, jobs, statuses)
		updated += syncInventoryPublicationReports(database, jobs)
	}
	return updated, nil
}

// syncMissingR2Completions closes jobs whose terminal runner entry aged out of
// the daemon's 24-hour state window while the controller was offline. Only
// previously-dispatched attempts are eligible; a brand-new queued row should
// not perform a speculative completion lookup before its first inbox write.
func syncMissingR2Completions(database *sql.DB, jobs []*db.Job, statuses map[int64]queueBatchStatus) int {
	updated := 0
	for _, job := range jobs {
		if job == nil || job.LastSyncedStatus == "" {
			continue
		}
		if _, observed := statuses[job.ID]; observed {
			continue
		}
		result, err := syncJobStatusFromR2ForBatch(database, job)
		if err != nil {
			continue
		}
		if result.Updated {
			updated++
		}
	}
	return updated
}

// applyBatchStatuses processes pre-fetched batch statuses for a set of jobs.
func applyBatchStatuses(database *sql.DB, jobIDs []int64, jobByID map[int64]*db.Job, statuses map[int64]queueBatchStatus, timeout time.Duration) (int, error) {
	var updated int
	for _, jobID := range jobIDs {
		job := jobByID[jobID]
		if job == nil {
			continue
		}
		status, ok := statuses[jobID]
		if !ok {
			continue
		}
		syncSourceExecutionMetadata(database, job, status)

		switch status.State {
		case queueStateQueued:
			recordQueueDispatchOK(database, job.ID)
			if job.PendingStatus != nil && (*job.PendingStatus == db.StatusCanceled || *job.PendingStatus == db.StatusKilled || *job.PendingStatus == db.StatusDead) {
				if err := removeFromQueueFile(job.Host, job.ID, timeout); err != nil {
					return updated, err
				}
				finalStatus := db.StatusCanceled
				if *job.PendingStatus == db.StatusKilled || *job.PendingStatus == db.StatusDead {
					finalStatus = db.StatusKilled
				}
				if err := db.ClearPendingAndUpdateStatus(database, job.ID, finalStatus); err != nil {
					return updated, err
				}
				updated++
				continue
			}
			if job.Status == db.StatusRunning {
				if err := db.MarkQueuedByID(database, job.ID); err != nil {
					return updated, err
				}
				updated++
			}
		case queueStateRunning:
			recordQueueDispatchOK(database, job.ID)
			if job.StartTime == 0 {
				if status.FromR2 && status.Mtime > 0 {
					if err := db.UpdateStartTime(database, job.ID, status.Mtime); err != nil {
						slog.Warn("failed to update start time from R2 runner state", "component", "sync", "job_id", job.ID, "error", err)
					} else {
						job.StartTime = status.Mtime
					}
				} else if _, err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
					slog.Warn("failed to update start time", "component", "sync", "job_id", job.ID, "error", err)
				}
			}
			switch job.Status {
			case db.StatusQueued:
				if err := db.MarkQueuedJobRunning(database, job.ID); err != nil {
					return updated, err
				}
				updated++
			case db.StatusStarting:
				if err := db.MarkRunningByID(database, job.ID); err != nil {
					return updated, err
				}
				updated++
			case db.StatusPaused:
				if err := db.MarkRunningFromPaused(database, job.ID); err != nil {
					return updated, err
				}
				updated++
			case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
				if err := db.MarkRunningFromTerminal(database, job.ID); err != nil {
					return updated, err
				}
				updated++
			}
			if status.GPUDevices != "" {
				syncGPUDevicesToMetadata(database, job, status.GPUDevices)
			}
		case queueStatePaused:
			recordQueueDispatchOK(database, job.ID)
			if job.StartTime == 0 {
				if _, err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
					slog.Warn("failed to update start time", "component", "sync", "job_id", job.ID, "error", err)
				}
			}
			switch job.Status {
			case db.StatusQueued, db.StatusStarting, db.StatusRunning:
				if err := db.MarkPausedByID(database, job.ID); err != nil {
					return updated, err
				}
				updated++
			case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
				if err := db.MarkPausedFromTerminal(database, job.ID); err != nil {
					return updated, err
				}
				updated++
			}
			if status.GPUDevices != "" {
				syncGPUDevicesToMetadata(database, job, status.GPUDevices)
			}
		case queueStateDead:
			if job.PendingStatus != nil && *job.PendingStatus == db.StatusRunning && job.Status == db.StatusQueued {
				if err := startQueuedJobNow(database, job, timeout); err != nil {
					return updated, err
				}
				updated++
				continue
			}
			if isQueuedAndActive(job) {
				continue
			}
			if job.Status != db.StatusDead && job.Status != db.StatusFailed && job.Status != db.StatusKilled && job.Status != db.StatusCanceled {
				if err := db.MarkDeadByID(database, job.ID); err != nil {
					return updated, err
				}
				updated++
			}
		case queueStatePreflightRejected:
			// The runner rejected the attempt before stamping a startTime
			// (currently: source provenance mismatch). Persist failure_reason
			// FIRST (while the attempt is still open — SetJobRemoteState
			// targets the latest open attempt), then close it without
			// exit_code or end_time. Record a dispatch-failed lifecycle event
			// so `weft job diagnose` surfaces the reason via
			// LatestInventoryDispatchBlock. The job stays in queued so the
			// autopilot / user can replan it.
			if status.FailureReason != "" {
				if err := db.SetJobRemoteState(database, job.ID, "", status.FailureReason); err != nil {
					slog.Warn("failed to record failure reason", "component", "sync", "job_id", job.ID, "error", err)
				}
				recordPreflightDispatchBlock(database, job.ID, status.FailureReason)
			}
			if err := db.CloseAttempt(database, job.ID, db.StatusFailed, nil, 0); err != nil {
				return updated, err
			}
			updated++
		default:
			if status.ExitCode != nil {
				if status.FromR2 {
					result, err := syncJobStatusFromR2(database, job)
					if err != nil {
						slog.Debug("R2 runner reported completion before result marker was readable", "component", "sync", "job_id", job.ID, "error", err)
						continue
					}
					if result.Updated {
						updated++
					}
					continue
				}
				if status.RunID != 0 && job.LatestRunID != nil && status.RunID != *job.LatestRunID {
					slog.Debug("ignoring stale completed queue status", "component", "sync", "job_id", job.ID, "remote_run_id", status.RunID, "latest_run_id", *job.LatestRunID)
					continue
				}
				recordQueueDispatchOK(database, job.ID)
				metaEndTime, _, metaErr := UpdateTimesFromMetadata(database, job, timeout)
				if metaErr != nil {
					return updated, metaErr
				}
				// Second evidence source: a failed/empty metadata read here is
				// silent, and this may be the only tick that ever records this
				// completion — without a start_time the attempt would keep
				// end_time but NULL start_time forever (seen live: wj3871–73).
				BackfillStartTimeFromCompletionRecord(database, job, timeout)
				// Prefer end_time from metadata (system clock) over status file mtime (NFS clock)
				endTime := status.Mtime
				if metaEndTime > 0 {
					endTime = metaEndTime
				}
				if err := RecordJobCompletion(database, job.ID, *status.ExitCode, endTime); err != nil {
					return updated, err
				}
				// Record failure reason if present, and surface as a dispatch
				// block so explain.ForJob shows the reason in `diagnose`.
				if status.FailureReason != "" {
					if err := db.SetJobRemoteState(database, job.ID, "", status.FailureReason); err != nil {
						slog.Warn("failed to record failure reason", "component", "sync", "job_id", job.ID, "error", err)
					}
					recordPreflightDispatchBlock(database, job.ID, status.FailureReason)
				}
				CacheCompletedJobLog(job, timeout)
				// Fetch resource usage data (best-effort). This writes only
				// telemetry/metadata (resource usage samples, phase timings) —
				// not job status — so a dropped write on lock contention is
				// harmless and is re-derived on the next sync pass. The
				// authoritative completion write above (RecordJobCompletion)
				// is the one that must not be lost.
				if _, err := updateJobResourceUsage(database, job, timeout); err != nil {
					slog.Warn("failed to update resource usage", "component", "sync", "job_id", job.ID, "error", err)
				}
				// Sync timeseries telemetry (best-effort)
				if err := syncJobTimeseries(database, job, timeout); err != nil {
					slog.Warn("failed to sync timeseries", "component", "sync", "job_id", job.ID, "error", err)
				}
				if err := syncJobTelemetry(database, job, timeout); err != nil {
					slog.Warn("failed to sync telemetry", "component", "sync", "job_id", job.ID, "error", err)
				}
				updated++
			}
		}
	}

	return updated, nil
}

// recordPreflightDispatchBlock inserts a dedup-windowed
// EventQueueDispatchFailed for a job whose runner reported a failure_reason.
// This is what `weft job diagnose` reads via
// explain.LatestInventoryDispatchBlock to surface the reason on a queued or
// just-failed job.
func recordPreflightDispatchBlock(database *sql.DB, jobID int64, detail string) {
	_, _ = db.InsertLifecycleEventDedup(database, &db.LifecycleEvent{
		EventKind: db.EventQueueDispatchFailed,
		JobID:     jobID,
		Detail:    truncateDispatchDetail(detail),
	}, dispatchEventDedupeWindow)
}

func fetchQueueBatchStatus(host string, jobIDs []int64, timeout time.Duration) (map[int64]queueBatchStatus, error) {
	if hostUsesR2Queue(host) {
		state, err := fetchR2RunnerState(host)
		if err != nil {
			return nil, err
		}
		results := make(map[int64]queueBatchStatus, len(jobIDs))
		wanted := make(map[int64]struct{}, len(jobIDs))
		for _, id := range jobIDs {
			wanted[id] = struct{}{}
		}
		for _, id := range state.Pending {
			if _, ok := wanted[id]; ok {
				results[id] = queueBatchStatus{State: queueStateQueued, FromR2: true}
			}
		}
		if state.Current != nil {
			if _, ok := wanted[*state.Current]; ok {
				results[*state.Current] = queueBatchStatus{State: queueStateRunning, FromR2: true}
			}
		}
		for idText, running := range state.Running {
			id, err := strconv.ParseInt(idText, 10, 64)
			if err != nil {
				continue
			}
			if _, ok := wanted[id]; ok {
				results[id] = queueBatchStatus{
					State: queueStateRunning, RunID: running.RunID, Mtime: running.StartedAt,
					GPUDevices: strings.Join(running.GPUDevices, ","), FromR2: true,
				}
			}
		}
		for idText, finished := range state.Finished {
			id, err := strconv.ParseInt(idText, 10, 64)
			if err != nil {
				continue
			}
			if _, ok := wanted[id]; ok {
				exitCode := finished.ExitCode
				results[id] = queueBatchStatus{ExitCode: &exitCode, Mtime: finished.FinishedAt, FromR2: true}
			}
		}
		return results, nil
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	idList := make([]string, 0, len(jobIDs))
	for _, id := range jobIDs {
		idList = append(idList, fmt.Sprintf("%d", id))
	}
	idsArg := strings.Join(idList, " ")

	agentPath := "~/.cache/weft/bin/weft-agent"
	cmd := fmt.Sprintf("%s batch-status %s", agentPath, idsArg)
	stdout, stderr, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
		if s := strings.TrimSpace(stderr); s != "" {
			return nil, fmt.Errorf("%w: %s", err, s)
		}
		return nil, err
	}

	results := make(map[int64]queueBatchStatus, len(jobIDs))
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "JOB|") {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 3 {
			continue
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		state := parts[2]
		switch state {
		case "COMPLETED":
			if len(parts) < 5 {
				continue
			}
			exitCode, err := strconv.Atoi(parts[3])
			if err != nil {
				continue
			}
			var mtime int64
			if parts[4] != "" {
				mtime, _ = strconv.ParseInt(parts[4], 10, 64)
			}
			var runID int64
			failureReason := ""
			if len(parts) >= 7 {
				if parts[5] != "" {
					runID, _ = strconv.ParseInt(parts[5], 10, 64)
				}
				if parts[6] != "" {
					failureReason = strings.TrimSpace(parts[6])
				}
			} else if len(parts) >= 6 && parts[5] != "" {
				// TODO(remove after 2026-06-12): legacy agents emitted failure_reason
				// in field 5 before COMPLETED lines included run_id.
				failureReason = strings.TrimSpace(parts[5])
			}
			status := queueBatchStatus{ExitCode: &exitCode, Mtime: mtime, RunID: runID, FailureReason: failureReason}
			status.Source = parseBatchSourceExecution(parts, 7)
			results[id] = status
		case "PREFLIGHT_REJECTED":
			// Format: JOB|<id>|PREFLIGHT_REJECTED|<ts>|<failure_reason>
			var mtime int64
			if len(parts) >= 4 && parts[3] != "" {
				mtime, _ = strconv.ParseInt(parts[3], 10, 64)
			}
			failureReason := ""
			if len(parts) >= 5 && parts[4] != "" {
				failureReason = strings.TrimSpace(parts[4])
			}
			agentVersion := ""
			if len(parts) >= 6 {
				agentVersion = strings.TrimSpace(parts[5])
			}
			results[id] = queueBatchStatus{State: queueStatePreflightRejected, Mtime: mtime, FailureReason: failureReason, AgentVersion: agentVersion}
		case "CURRENT", "RUNNING":
			gpuDevs := ""
			if len(parts) >= 4 {
				gpuDevs = parts[3]
			}
			results[id] = queueBatchStatus{State: queueStateRunning, GPUDevices: gpuDevs, Source: parseBatchSourceExecution(parts, 4)}
		case "PAUSED":
			gpuDevs := ""
			if len(parts) >= 4 {
				gpuDevs = parts[3]
			}
			results[id] = queueBatchStatus{State: queueStatePaused, GPUDevices: gpuDevs, Source: parseBatchSourceExecution(parts, 4)}
		case "QUEUED":
			results[id] = queueBatchStatus{State: queueStateQueued}
		case "DEAD":
			results[id] = queueBatchStatus{State: queueStateDead}
		}
	}

	return results, nil
}

func parseBatchSourceExecution(parts []string, offset int) *db.JobSourceExecutionMetadata {
	if len(parts) < offset+8 || strings.TrimSpace(parts[offset]) == "" {
		return nil
	}
	verifiedAt, _ := strconv.ParseInt(parts[offset+5], 10, 64)
	rootCount, _ := strconv.Atoi(parts[offset+7])
	return &db.JobSourceExecutionMetadata{
		DispatchMode:     strings.TrimSpace(parts[offset]),
		IdentityKind:     strings.TrimSpace(parts[offset+1]),
		DispatchedSHA256: strings.TrimSpace(parts[offset+2]),
		VerifiedSHA256:   strings.TrimSpace(parts[offset+3]),
		Verification:     strings.TrimSpace(parts[offset+4]),
		VerifiedAt:       verifiedAt,
		AgentVersion:     strings.TrimSpace(parts[offset+6]),
		RootCount:        rootCount,
	}
}

func syncSourceExecutionMetadata(database *sql.DB, job *db.Job, status queueBatchStatus) {
	if job == nil {
		return
	}
	execution := status.Source
	if execution == nil && status.State == queueStatePreflightRejected && job.Metadata != nil && job.Metadata.Source != nil && job.Metadata.Source.Pin != nil {
		pin := job.Metadata.Source.Pin
		verification := db.SourceVerificationPending
		switch {
		case strings.Contains(status.FailureReason, "mismatch"):
			verification = db.SourceVerificationMismatch
		case strings.Contains(status.FailureReason, "unavailable"), strings.Contains(status.FailureReason, "fetch_failed"):
			verification = db.SourceVerificationObjectsUnavailable
		}
		execution = &db.JobSourceExecutionMetadata{
			DispatchMode:     "pinned_inventory_manifest",
			IdentityKind:     db.SourceIdentityManifestV2,
			DispatchedSHA256: pin.Hash,
			Verification:     verification,
			RootCount:        len(pin.Roots),
			AgentVersion:     status.AgentVersion,
		}
	}
	if execution == nil {
		return
	}
	copy := *execution
	if job.Metadata != nil && job.Metadata.Source != nil && job.Metadata.Source.Pin != nil {
		copy.SubmittedIdentityKind = db.SourceIdentityManifestV2
		copy.SubmittedSHA256 = job.Metadata.Source.Pin.Hash
		if copy.IdentityKind == copy.SubmittedIdentityKind {
			if copy.DispatchedSHA256 != copy.SubmittedSHA256 || (copy.VerifiedSHA256 != "" && copy.VerifiedSHA256 != copy.SubmittedSHA256) {
				copy.Verification = db.SourceVerificationMismatch
			}
		}
	}
	meta := job.Metadata
	if meta == nil {
		meta = &db.JobMetadata{}
	}
	if meta.Source == nil {
		meta.Source = &db.JobSourceMetadata{}
	}
	if reflect.DeepEqual(meta.Source.Execution, &copy) {
		return
	}
	meta.Source.Execution = &copy
	if err := db.SetJobMetadata(database, job.ID, meta); err != nil {
		slog.Warn("failed to update source execution metadata", "component", "sync", "job_id", job.ID, "error", err)
		return
	}
	job.Metadata = meta
}

// syncGPUDevicesToMetadata persists the assigned GPU devices to the job's metadata
// if they differ from what's already stored.
func syncGPUDevicesToMetadata(database *sql.DB, job *db.Job, gpuDevices string) {
	meta := job.Metadata
	if meta == nil {
		meta = &db.JobMetadata{}
	}
	if meta.Resource == nil {
		meta.Resource = &db.ResourceUsage{}
	}
	if meta.Resource.GPUDevices != gpuDevices {
		meta.Resource.GPUDevices = gpuDevices
		if err := db.SetJobMetadata(database, job.ID, meta); err != nil {
			slog.Warn("failed to update GPU devices metadata", "component", "sync", "job_id", job.ID, "error", err)
		}
		job.Metadata = meta
	}
}
