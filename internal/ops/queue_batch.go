package ops

import (
	"database/sql"
	"fmt"
	"log"
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
	GPUDevices    string
	FailureReason string
}

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
		return 0, err
	}

	return applyBatchStatuses(database, jobIDs, jobByID, statuses, timeout)
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

		switch status.State {
		case queueStateQueued:
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
			if job.StartTime == 0 {
				if _, err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
					log.Printf("sync: failed to update start time for job %d: %v", job.ID, err)
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
			if job.StartTime == 0 {
				if _, err := UpdateStartTimeFromMetadata(database, job, timeout); err != nil {
					log.Printf("sync: failed to update start time for job %d: %v", job.ID, err)
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
		default:
			if status.ExitCode != nil {
				metaEndTime, _, metaErr := UpdateTimesFromMetadata(database, job, timeout)
				if metaErr != nil {
					return updated, metaErr
				}
				// Prefer end_time from metadata (system clock) over status file mtime (NFS clock)
				endTime := status.Mtime
				if metaEndTime > 0 {
					endTime = metaEndTime
				}
				if err := RecordJobCompletion(database, job.ID, *status.ExitCode, endTime); err != nil {
					return updated, err
				}
				// Record failure reason if present
				if status.FailureReason != "" {
					if err := db.SetJobRemoteState(database, job.ID, "", status.FailureReason); err != nil {
						log.Printf("sync: failed to record failure reason for job %d: %v", job.ID, err)
					}
				}
				CacheCompletedJobLog(job, timeout)
				// Fetch resource usage data (best-effort)
				if _, err := updateJobResourceUsage(database, job, timeout); err != nil {
					log.Printf("sync: failed to update resource usage for job %d: %v", job.ID, err)
				}
				// Sync timeseries telemetry (best-effort)
				if err := syncJobTimeseries(database, job, timeout); err != nil {
					log.Printf("sync: failed to sync timeseries for job %d: %v", job.ID, err)
				}
				if err := syncJobTelemetry(database, job, timeout); err != nil {
					log.Printf("sync: failed to sync telemetry for job %d: %v", job.ID, err)
				}
				updated++
			}
		}
	}

	return updated, nil
}

func fetchQueueBatchStatus(host string, jobIDs []int64, timeout time.Duration) (map[int64]queueBatchStatus, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	idList := make([]string, 0, len(jobIDs))
	for _, id := range jobIDs {
		idList = append(idList, fmt.Sprintf("%d", id))
	}
	idsArg := strings.Join(idList, " ")

	agentPath := "~/.cache/weft/bin/weft-agent"
	cmd := fmt.Sprintf("%s batch-status --queue %s %s", agentPath, DefaultQueueName, idsArg)
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
			failureReason := ""
			if len(parts) >= 6 && parts[5] != "" {
				failureReason = strings.TrimSpace(parts[5])
			}
			results[id] = queueBatchStatus{ExitCode: &exitCode, Mtime: mtime, FailureReason: failureReason}
		case "CURRENT", "RUNNING":
			gpuDevs := ""
			if len(parts) >= 4 {
				gpuDevs = parts[3]
			}
			results[id] = queueBatchStatus{State: queueStateRunning, GPUDevices: gpuDevs}
		case "PAUSED":
			gpuDevs := ""
			if len(parts) >= 4 {
				gpuDevs = parts[3]
			}
			results[id] = queueBatchStatus{State: queueStatePaused, GPUDevices: gpuDevs}
		case "QUEUED":
			results[id] = queueBatchStatus{State: queueStateQueued}
		case "DEAD":
			results[id] = queueBatchStatus{State: queueStateDead}
		}
	}

	return results, nil
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
			log.Printf("sync: failed to update GPU devices metadata for job %d: %v", job.ID, err)
		}
		job.Metadata = meta
	}
}
