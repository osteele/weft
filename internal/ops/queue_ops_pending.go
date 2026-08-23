package ops

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/secrets"
	"github.com/osteele/weft/internal/ssh"
)

var errQueueConnection = errors.New("queue connection error")

// QueuePriorityResult reports the outcome of a move-to-front request.
type QueuePriorityResult struct {
	Moved    bool
	Deferred bool
}

func queueOpTimeout(opts ExecuteOptions) time.Duration {
	if opts.Timeout > 0 {
		return opts.Timeout
	}
	return TimeoutNormal.Duration()
}

func queueEnvVarsForJob(job *db.Job, envVars []string) []string {
	if job == nil {
		return nil
	}
	if envVars == nil {
		envVars = job.EnvVars
	}
	merged := append([]string(nil), envVars...)
	// Only inject CUDA_VISIBLE_DEVICES for explicit GPU device jobs.
	// GPU class-based jobs resolve the device at runtime on the remote host.
	if job.GPU != "" && job.GPUClass == "" && !hasCUDAEnvVar(merged) {
		merged = append(merged, "CUDA_VISIBLE_DEVICES="+job.GPU)
	}
	return merged
}

func hasCUDAEnvVar(envVars []string) bool {
	for _, ev := range envVars {
		if strings.HasPrefix(ev, "CUDA_VISIBLE_DEVICES=") {
			return true
		}
	}
	return false
}

func queueEntryForJob(job *db.Job, envVars []string, depSpec string) opsqueue.QueueEntry {
	return opsqueue.QueueEntry{
		JobID:        job.ID,
		WorkingDir:   job.WorkingDir,
		Command:      job.Command,
		Description:  job.Description,
		EnvVars:      queueEnvVarsForJob(job, envVars),
		DepSpec:      depSpec,
		CPUAllotment: job.CPUAllotment,
		GPU:          job.GPU,
		GPUClass:     job.GPUClass,
		GPUCount:     job.RequestedGPUCount(),
		GPUMemGB:     job.GPUMemGB,
		Interconnect: job.RequestedInterconnect(),
		CPUCores:     job.RequestedCPUCores(),
		Tags:         job.Tags,
		OutputDirs:   job.OutputDirs,
		Outputs:      job.Outputs,
		Produces:     job.Produces,
		Needs:        job.Needs,
	}
}

func writeQueueJobFile(host string, entry opsqueue.QueueEntry, timeout time.Duration) error {
	resolvedEnv, err := secrets.ResolveEnvVars(entry.EnvVars)
	if err != nil {
		return err
	}
	entry.EnvVars = resolvedEnv
	job := opsqueue.CommandJob{
		ID:       entry.JobID,
		Dir:      entry.WorkingDir,
		Cmd:      entry.Command,
		Desc:     entry.Description,
		Env:      entry.EnvVars,
		Deps:     entry.DepSpec,
		CPU:      entry.CPUAllotment,
		GPU:      entry.GPU,
		GPUClass: entry.GPUClass,
		GPUMem:   entry.GPUMemGB,
		Tags:     entry.Tags,
		Produces: entry.Produces,
		Needs:    entry.Needs,
	}
	payload, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal queue job: %w", err)
	}

	jobFile := fmt.Sprintf("%s/job-%d.json", opsqueue.QueueDir, entry.JobID)
	cmd := fmt.Sprintf(`mkdir -p %s && printf '%%s\n' %q > %s`, opsqueue.QueueDir, string(payload), jobFile)

	var stderr string
	if timeout > 0 {
		_, stderr, err = ssh.RunWithTimeout(host, cmd, timeout)
	} else {
		_, stderr, err = ssh.Run(host, cmd)
	}
	if err != nil {
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return fmt.Errorf("%w: %s", errQueueConnection, strings.TrimSpace(stderr))
		}
		return fmt.Errorf("write queue job file: %s", ssh.FriendlyError(host, stderr, err))
	}
	return nil
}

func applyQueueUpdate(job *db.Job, envVars []string, depSpec string, timeout time.Duration) error {
	if job == nil {
		return fmt.Errorf("job is nil")
	}
	entry := queueEntryForJob(job, envVars, depSpec)
	if hostUsesR2Queue(job.Host) {
		resolvedEnv, err := secrets.ResolveEnvVars(entry.EnvVars)
		if err != nil {
			return err
		}
		entry.EnvVars = resolvedEnv
		if job.LatestRunID != nil {
			entry.RunID = *job.LatestRunID
		}
		manifest, pinned, err := pinnedQueueSourceManifest(job)
		if err != nil {
			return err
		}
		if pinned {
			entry.SourceManifest = manifest
			entry.SourceSHA256 = manifest.SHA256
			entry.SourceR2Key = dataplane.SourceClosureReceiptV2(manifest.SHA256)
		}
		return appendQueueCommand(job.Host, opsqueue.NewAddCommand(entry), opsqueue.AppendCommandOptions{Timeout: timeout})
	}
	return writeQueueJobFile(job.Host, entry, timeout)
}

func applyQueuePriority(host string, jobID int64, timeout time.Duration) (bool, error) {
	if hostUsesR2Queue(host) {
		state, err := fetchR2RunnerState(host)
		if err != nil {
			return false, fmt.Errorf("%w: %v", errQueueConnection, err)
		}
		position := -1
		for i, id := range state.Pending {
			if id == jobID {
				position = i
				break
			}
		}
		if position < 0 {
			return false, fmt.Errorf("job %s not found in queue", ids.FormatJobID(jobID))
		}
		if position == 0 {
			return false, nil
		}
		cmd := opsqueue.NewPriorityCommand(jobID)
		if err := appendQueueCommand(host, cmd, opsqueue.AppendCommandOptions{Timeout: timeout}); err != nil {
			return false, err
		}
		return true, nil
	}
	stateFile := opsqueue.StateFilePath()
	checkCmd := fmt.Sprintf("jq -e '.pending | index(%d) != null' %s 2>/dev/null && echo YES || echo NO", jobID, stateFile)
	stdout, stderr, err := ssh.RunWithTimeout(host, checkCmd, timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) || ssh.IsConnectionError(err.Error()) {
			return false, fmt.Errorf("%w: %s", errQueueConnection, strings.TrimSpace(stderr))
		}
		return false, fmt.Errorf("check job %s in queue: %s", ids.FormatJobID(jobID), ssh.FriendlyError(host, stderr, err))
	}

	result := strings.TrimSpace(stdout)
	if result != "YES" {
		return false, fmt.Errorf("job %s not found in queue", ids.FormatJobID(jobID))
	}

	frontCmd := fmt.Sprintf("jq -r '.pending[0] // \"\"' %s 2>/dev/null", stateFile)
	frontStdout, _, _ := ssh.RunWithTimeout(host, frontCmd, timeout)
	if strings.TrimSpace(frontStdout) == fmt.Sprintf("%d", jobID) {
		return false, nil
	}

	cmd := opsqueue.NewPriorityCommand(jobID)
	if err := appendQueueCommand(host, cmd, opsqueue.AppendCommandOptions{Timeout: timeout}); err != nil {
		if isQueueConnectionError(err) {
			return false, fmt.Errorf("%w: %s", errQueueConnection, err.Error())
		}
		return false, err
	}

	return true, nil
}

func isQueueConnectionError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, errQueueConnection) {
		return true
	}
	var qaErr *opsqueue.QueueAppendError
	if errors.As(err, &qaErr) && qaErr.IsConnectionError() {
		return true
	}
	return ssh.IsConnectionError(err.Error())
}

func ensureDeferredQueueOp(database *sql.DB, job *db.Job, op string) error {
	if job == nil {
		return fmt.Errorf("job is nil")
	}
	pending, err := db.HasPendingOperation(database, job.ID, op)
	if err != nil {
		return err
	}
	if pending {
		return nil
	}
	return db.AddDeferredOperation(database, job.Host, op, job.ID, "")
}

// RequestQueueUpdate records a queue update operation and attempts to apply it immediately.
// If the host is unreachable, the operation remains pending for the next sync.
func RequestQueueUpdate(database *sql.DB, job *db.Job, opts ExecuteOptions) (Result, error) {
	if job == nil {
		return Result{}, fmt.Errorf("job is nil")
	}
	if job.EffectiveStatus() != db.StatusQueued {
		return Result{}, fmt.Errorf("job %s (status: %s): %w", ids.FormatJobID(job.ID), job.EffectiveStatus(), ErrNotQueued)
	}
	if !job.UsesQueueRunner() {
		return Result{}, fmt.Errorf("queue updates only supported for queue-runner jobs")
	}

	if err := ensureDeferredQueueOp(database, job, db.OpUpdateQueuedJob); err != nil {
		return Result{}, err
	}

	timeout := queueOpTimeout(opts)
	if err := applyQueueUpdate(job, job.EnvVars, job.DepSpec, timeout); err != nil {
		if isQueueConnectionError(err) {
			return Result{
				Success:  true,
				Deferred: true,
				JobID:    job.ID,
				Message:  fmt.Sprintf("Job %s update pending (host unreachable)", ids.FormatJobID(job.ID)),
			}, nil
		}
		if delErr := db.DeletePendingOperation(database, job.ID, db.OpUpdateQueuedJob); delErr != nil {
			slog.Warn("failed to delete pending op", "component", "sync", "job_id", job.ID, "error", delErr)
		}
		return Result{}, err
	}

	if err := db.DeletePendingOperation(database, job.ID, db.OpUpdateQueuedJob); err != nil {
		slog.Warn("failed to delete pending op", "component", "sync", "job_id", job.ID, "error", err)
	}
	return Result{
		Success: true,
		JobID:   job.ID,
		Message: fmt.Sprintf("Job %s updated in queue", ids.FormatJobID(job.ID)),
	}, nil
}

// RequestQueuePriority records a move-to-front request and attempts to apply it immediately.
// If the host is unreachable, the operation remains pending for the next sync.
func RequestQueuePriority(database *sql.DB, job *db.Job, opts ExecuteOptions) (QueuePriorityResult, error) {
	if job == nil {
		return QueuePriorityResult{}, fmt.Errorf("job is nil")
	}
	if job.EffectiveStatus() != db.StatusQueued {
		return QueuePriorityResult{}, fmt.Errorf("job %s (status: %s): %w", ids.FormatJobID(job.ID), job.EffectiveStatus(), ErrNotQueued)
	}
	if !job.UsesQueueRunner() {
		return QueuePriorityResult{}, fmt.Errorf("queue priority only supported for queue-runner jobs")
	}

	if err := ensureDeferredQueueOp(database, job, db.OpMoveToFront); err != nil {
		return QueuePriorityResult{}, err
	}

	timeout := queueOpTimeout(opts)
	moved, err := applyQueuePriority(job.Host, job.ID, timeout)
	if err != nil {
		if isQueueConnectionError(err) {
			return QueuePriorityResult{Deferred: true}, nil
		}
		if delErr := db.DeletePendingOperation(database, job.ID, db.OpMoveToFront); delErr != nil {
			slog.Warn("failed to delete pending op", "component", "sync", "job_id", job.ID, "error", delErr)
		}
		return QueuePriorityResult{}, err
	}

	if err := db.DeletePendingOperation(database, job.ID, db.OpMoveToFront); err != nil {
		slog.Warn("failed to delete pending op", "component", "sync", "job_id", job.ID, "error", err)
	}
	if moved {
		if err := db.SetQueuedAtBefore(database, job.ID, job.Host); err != nil {
			slog.Warn("failed to update queued_at", "component", "sync", "job_id", job.ID, "error", err)
		}
	}
	return QueuePriorityResult{Moved: moved}, nil
}

// ProcessDeferredQueueOps applies pending queue operations for a host.
// Connection failures are treated as deferred work and do not return an error.
func ProcessDeferredQueueOps(database *sql.DB, host string, timeout time.Duration) (SyncResult, error) {
	ops, err := db.GetDeferredOperations(database, host)
	if err != nil {
		return SyncResult{}, err
	}

	var result SyncResult
	for _, op := range ops {
		switch op.Operation {
		case db.OpRemoveQueued:
			job, err := db.GetJobByID(database, op.JobID)
			if err != nil {
				return result, err
			}
			if job == nil {
				_ = db.DeleteDeferredOperation(database, op.ID)
				continue
			}
			// If the job has been assigned back to this host, the cleanup request is stale.
			if job.Host == host {
				_ = db.DeleteDeferredOperation(database, op.ID)
				continue
			}
			cleanupJob := *job
			cleanupJob.Host = host
			if err := applyCancelToRemote(&cleanupJob, timeout); err != nil {
				if isQueueConnectionError(err) {
					return result, nil
				}
				return result, err
			}
			if err := db.DeleteDeferredOperation(database, op.ID); err != nil {
				slog.Warn("failed to delete deferred op", "component", "sync", "op_id", op.ID, "error", err)
			}
			result.HostContacted = true
		case db.OpUpdateQueuedJob:
			job, err := db.GetJobByID(database, op.JobID)
			if err != nil {
				return result, err
			}
			if job == nil || job.Host != host || job.EffectiveStatus() != db.StatusQueued {
				_ = db.DeleteDeferredOperation(database, op.ID)
				continue
			}
			if err := applyQueueUpdate(job, job.EnvVars, job.DepSpec, timeout); err != nil {
				if isQueueConnectionError(err) {
					return result, nil
				}
				return result, err
			}
			if err := db.DeleteDeferredOperation(database, op.ID); err != nil {
				slog.Warn("failed to delete deferred op", "component", "sync", "op_id", op.ID, "error", err)
			}
			result.HostContacted = true
		case db.OpMoveToFront:
			job, err := db.GetJobByID(database, op.JobID)
			if err != nil {
				return result, err
			}
			if job == nil || job.Host != host || job.EffectiveStatus() != db.StatusQueued {
				_ = db.DeleteDeferredOperation(database, op.ID)
				continue
			}
			moved, err := applyQueuePriority(job.Host, job.ID, timeout)
			if err != nil {
				if isQueueConnectionError(err) {
					return result, nil
				}
				return result, err
			}
			if moved {
				if err := db.SetQueuedAtBefore(database, job.ID, job.Host); err != nil {
					slog.Warn("failed to update queued_at", "component", "sync", "job_id", job.ID, "error", err)
				}
			}
			if err := db.DeleteDeferredOperation(database, op.ID); err != nil {
				slog.Warn("failed to delete deferred op", "component", "sync", "op_id", op.ID, "error", err)
			}
			result.HostContacted = true
		default:
			continue
		}
	}

	return result, nil
}
