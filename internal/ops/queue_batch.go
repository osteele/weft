package ops

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/session"
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
				_ = UpdateStartTimeFromMetadata(database, job, timeout)
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
				_ = UpdateStartTimeFromMetadata(database, job, timeout)
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
			if job.Status != db.StatusDead && job.Status != db.StatusFailed && job.Status != db.StatusKilled && job.Status != db.StatusCanceled {
				if err := db.MarkDeadByID(database, job.ID); err != nil {
					return updated, err
				}
				updated++
			}
		default:
			if status.ExitCode != nil {
				metaEndTime, metaErr := UpdateTimesFromMetadata(database, job, timeout)
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
					_ = db.SetJobRemoteState(database, job.ID, "", status.FailureReason)
				}
				CacheCompletedJobLog(job, timeout)
				// Fetch resource usage data (best-effort)
				_, _ = updateJobResourceUsage(database, job, timeout)
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
	stateFile := fmt.Sprintf("~/.cache/weft/queue/%s.state.json", DefaultQueueName)
	statusPattern := fmt.Sprintf("%s/$id*.status", session.LogDir)
	pidPattern := fmt.Sprintf("%s/$id*.pid", session.LogDir)
	failureReasonPattern := fmt.Sprintf("%s/$id*.failure_reason", session.LogDir)
	script := fmt.Sprintf(`
STATE_FILE=%s
CURRENT=""
FINISHED="{}"
RUNNING_STATE="{}"
if [ -f "$STATE_FILE" ]; then
	CURRENT=$(jq -r '.current // ""' "$STATE_FILE" 2>/dev/null || echo "")
	PENDING=$(jq -r '.pending[]?' "$STATE_FILE" 2>/dev/null || true)
	FINISHED=$(jq -c '.finished // {}' "$STATE_FILE" 2>/dev/null || echo "{}")
	RUNNING_STATE=$(jq -c '.running // {}' "$STATE_FILE" 2>/dev/null || echo "{}")
else
	PENDING=""
fi
declare -A pending_map
for id in $PENDING; do
	pending_map[$id]=1
done
gpu_devs_for() {
	jq -r --arg id "$1" '.[$id].gpu_devices // [] | join(",")' <<< "$RUNNING_STATE" 2>/dev/null
}
read_failure_reason() {
	local fr_file=$(ls %s 2>/dev/null | head -1)
	if [ -n "$fr_file" ]; then
		cat "$fr_file" 2>/dev/null | head -1
	fi
}
	for id in %s; do
		finished_exit=$(echo "$FINISHED" | jq -r --arg id "$id" '.[$id].exit_code // empty' 2>/dev/null)
	finished_at=$(echo "$FINISHED" | jq -r --arg id "$id" '.[$id].finished_at // empty' 2>/dev/null)
	if [ -n "$finished_exit" ]; then
		fr=$(read_failure_reason)
		echo "JOB|$id|COMPLETED|$finished_exit|$finished_at|$fr"
		continue
	fi
		status_file=$(ls %s 2>/dev/null | head -1)
	if [ -n "$status_file" ]; then
		exit_code=$(cat "$status_file" 2>/dev/null | head -1)
		mtime=$(stat -c %%Y "$status_file" 2>/dev/null || stat -f %%m "$status_file" 2>/dev/null)
		fr=$(read_failure_reason)
		echo "JOB|$id|COMPLETED|$exit_code|$mtime|$fr"
		continue
	fi
	if [ "$CURRENT" = "$id" ]; then
		GPU_DEVS=$(gpu_devs_for "$id")
		pid_file=$(ls %s 2>/dev/null | head -1)
		if [ -n "$pid_file" ]; then
			pid=$(cat "$pid_file" 2>/dev/null | head -1)
			if [ -n "$pid" ]; then
				state=$(ps -o stat= -p $pid 2>/dev/null | tr -d ' ')
				if [ -n "$state" ]; then
					case "$state" in
						*T*) echo "JOB|$id|PAUSED|$GPU_DEVS" ;;
						*) echo "JOB|$id|CURRENT|$GPU_DEVS" ;;
					esac
					continue
				fi
			fi
		fi
		echo "JOB|$id|CURRENT|$GPU_DEVS"
		continue
	fi
	if [ -n "${pending_map[$id]+x}" ]; then
		echo "JOB|$id|QUEUED"
		continue
	fi
	GPU_DEVS=$(gpu_devs_for "$id")
	pid_file=$(ls %s 2>/dev/null | head -1)
	if [ -n "$pid_file" ]; then
		pid=$(cat "$pid_file" 2>/dev/null | head -1)
		if [ -n "$pid" ]; then
			state=$(ps -o stat= -p $pid 2>/dev/null | tr -d ' ')
			if [ -n "$state" ]; then
				case "$state" in
					*T*) echo "JOB|$id|PAUSED|$GPU_DEVS" ;;
					*) echo "JOB|$id|RUNNING|$GPU_DEVS" ;;
				esac
				continue
			fi
		fi
	fi
	echo "JOB|$id|DEAD"
done
`, stateFile, failureReasonPattern, idsArg, statusPattern, pidPattern, pidPattern)

	cmd := fmt.Sprintf("bash -c %s", ssh.EscapeForSingleQuotes(script))
	stdout, _, err := ssh.RunWithTimeout(host, cmd, timeout)
	if err != nil {
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
		_ = db.SetJobMetadata(database, job.ID, meta)
		job.Metadata = meta
	}
}
