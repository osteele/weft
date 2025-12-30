// Package ops provides unified job operation functions used by both CLI and TUI.
// All operations follow the same pattern: queue the operation first, then drain
// the queue until that operation is processed (or host becomes unreachable).
package ops

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// Result represents the outcome of an operation
type Result struct {
	Success  bool   // Operation completed successfully
	Deferred bool   // Operation was queued for later (host unreachable)
	JobID    int64  // Job ID (for create/restart operations)
	Message  string // Human-readable result message
}

// ExecuteOptions configures operation execution
type ExecuteOptions struct {
	Timeout time.Duration // SSH timeout (default 30s)
	Verbose bool          // Print verbose output
}

// DefaultOptions returns default execution options
func DefaultOptions() ExecuteOptions {
	return ExecuteOptions{
		Timeout: 30 * time.Second,
		Verbose: false,
	}
}

// ExecutionResult represents the result of executing all deferred operations for a host
type ExecutionResult struct {
	Completed int      // Number of operations completed
	Failed    int      // Number of operations that failed
	Errors    []string // Error messages for failed operations
}

// ExecuteAllDeferredOperations executes all pending deferred operations for a host.
// This is called during sync to process queued operations when the host becomes reachable.
// Returns the number of operations completed and any errors encountered.
func ExecuteAllDeferredOperations(database *sql.DB, host string, opts ExecuteOptions) (ExecutionResult, error) {
	operations, err := db.GetDeferredOperations(database, host)
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("get deferred operations: %w", err)
	}

	if len(operations) == 0 {
		return ExecutionResult{}, nil
	}

	var result ExecutionResult
	for _, op := range operations {
		_, err := executeOperation(database, host, op, opts)
		if err != nil {
			if isConnectionError(err) {
				// Host went offline - stop processing, remaining ops stay queued
				return result, nil
			}
			// Log error but continue with other operations
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("%s for job %d: %v", op.Operation, op.JobID, err))
			continue
		}

		// Delete completed operation
		if err := db.DeleteDeferredOperation(database, op.ID); err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("delete operation %d: %v", op.ID, err))
		}

		result.Completed++
	}

	return result, nil
}

// QueueAndExecute queues an operation and attempts to execute it immediately.
// If the host is reachable, the operation is executed and removed from the queue.
// If the host is unreachable, the operation remains queued for later sync.
func QueueAndExecute(database *sql.DB, host, operation string, jobID int64, queueName, payload string, opts ExecuteOptions) (Result, error) {
	// 1. Check if operation already pending (idempotent)
	hasPending, err := db.HasPendingOperation(database, jobID, operation)
	if err != nil {
		return Result{}, fmt.Errorf("check pending operations: %w", err)
	}

	var opID int64
	if !hasPending {
		// 2. Add operation to deferred queue
		opID, err = db.AddDeferredOperationReturningID(database, host, operation, jobID, queueName, payload)
		if err != nil {
			return Result{}, fmt.Errorf("queue operation: %w", err)
		}
	}

	// 3. Try to drain operations for this host up to and including this one
	result, err := drainOperationsUntil(database, host, opID, opts)
	if err != nil {
		// If drain failed due to connection, operation is still queued
		if isConnectionError(err) {
			return Result{
				Success:  false,
				Deferred: true,
				JobID:    jobID,
				Message:  fmt.Sprintf("Host %s unreachable, operation queued for next sync", host),
			}, nil
		}
		return Result{}, err
	}

	return result, nil
}

// drainOperationsUntil processes deferred operations for a host until the target operation is reached
func drainOperationsUntil(database *sql.DB, host string, targetOpID int64, opts ExecuteOptions) (Result, error) {
	ops, err := db.GetDeferredOperations(database, host)
	if err != nil {
		return Result{}, fmt.Errorf("get deferred operations: %w", err)
	}

	var lastResult Result
	for _, op := range ops {
		result, err := executeOperation(database, host, op, opts)
		if err != nil {
			if isConnectionError(err) {
				// Host went offline - stop draining, remaining ops stay queued
				return Result{
					Success:  false,
					Deferred: true,
					Message:  fmt.Sprintf("Host %s became unreachable, remaining operations queued", host),
				}, nil
			}
			// Continue with other operations (error is silently ignored for TUI compatibility)
		}

		// Delete completed operation (errors silently ignored)
		_ = db.DeleteDeferredOperation(database, op.ID)

		lastResult = result

		// Stop if we've processed the target operation
		if op.ID == targetOpID {
			break
		}
	}

	return lastResult, nil
}

// executeOperation executes a single deferred operation
func executeOperation(database *sql.DB, host string, op *db.DeferredOperation, opts ExecuteOptions) (Result, error) {
	switch op.Operation {
	case db.OpKillJob:
		return executeKill(database, host, op, opts)
	case db.OpRunJob:
		return executeRun(database, host, op, opts)
	case db.OpRestartJob:
		return executeRestart(database, host, op, opts)
	case db.OpRemoveQueued:
		return executeRemoveQueued(host, op, opts)
	case db.OpMoveFromQueue:
		return executeMoveFromQueue(host, op, opts)
	case db.OpQueueJob:
		return executeQueueAdd(database, op, opts)
	case db.OpStartQueuedJob:
		return executeStartQueued(database, op, opts)
	case db.OpUpdateQueuedJob:
		return executeUpdateQueued(database, op, opts)
	default:
		return Result{}, fmt.Errorf("unknown operation: %s", op.Operation)
	}
}

// isConnectionError checks if an error is due to SSH connection failure
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	return ssh.IsConnectionError(err.Error())
}

// executeRemoveQueued removes a job from the remote queue file
func executeRemoveQueued(host string, op *db.DeferredOperation, opts ExecuteOptions) (Result, error) {
	queueName := op.QueueName
	if queueName == "" {
		queueName = "default"
	}
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	removeCmd := fmt.Sprintf("sed -i '/^%d\t/d' %s 2>/dev/null || true", op.JobID, queueFile)
	_, stderr, err := ssh.RunWithTimeout(host, removeCmd, opts.Timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return Result{}, fmt.Errorf("connection error: %s", stderr)
		}
		return Result{}, err
	}
	return Result{
		Success: true,
		JobID:   op.JobID,
		Message: fmt.Sprintf("Job %d removed from queue", op.JobID),
	}, nil
}

// executeMoveFromQueue removes a job from the old host's queue file (for job move operations)
func executeMoveFromQueue(host string, op *db.DeferredOperation, opts ExecuteOptions) (Result, error) {
	queueName := op.QueueName
	if queueName == "" {
		queueName = "default"
	}
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	removeCmd := fmt.Sprintf("sed -i '/^%d\t/d' %s 2>/dev/null || true", op.JobID, queueFile)
	_, stderr, err := ssh.RunWithTimeout(host, removeCmd, opts.Timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return Result{}, fmt.Errorf("connection error: %s", stderr)
		}
		return Result{}, err
	}
	return Result{
		Success: true,
		JobID:   op.JobID,
		Message: fmt.Sprintf("Job %d removed from old queue", op.JobID),
	}, nil
}

// executeQueueAdd adds a job to the remote queue file
func executeQueueAdd(database *sql.DB, op *db.DeferredOperation, opts ExecuteOptions) (Result, error) {
	// Parse payload
	var payload struct {
		WorkingDir  string   `json:"working_dir"`
		Command     string   `json:"command"`
		Description string   `json:"description"`
		EnvVars     []string `json:"env_vars"`
		DepSpec     string   `json:"dep_spec"`
	}
	if err := json.Unmarshal([]byte(op.Payload), &payload); err != nil {
		return Result{}, fmt.Errorf("parse payload: %w", err)
	}

	queueName := op.QueueName
	if queueName == "" {
		queueName = DefaultQueueName
	}

	entry := QueueEntry{
		JobID:       op.JobID,
		WorkingDir:  payload.WorkingDir,
		Command:     payload.Command,
		Description: payload.Description,
		EnvVars:     payload.EnvVars,
		DepSpec:     payload.DepSpec,
	}

	appendOpts := AppendQueueEntryOptions{Timeout: opts.Timeout}
	if err := AppendQueueEntry(op.Host, queueName, entry, appendOpts); err != nil {
		var qaErr *QueueAppendError
		if errors.As(err, &qaErr) && qaErr.IsConnectionError() {
			return Result{}, fmt.Errorf("connection error: %s", qaErr.Stderr)
		}
		return Result{}, err
	}

	return Result{
		Success: true,
		JobID:   op.JobID,
		Message: fmt.Sprintf("Job %d added to queue '%s'", op.JobID, queueName),
	}, nil
}

// executeStartQueued starts a queued job via the queue runner
func executeStartQueued(database *sql.DB, op *db.DeferredOperation, opts ExecuteOptions) (Result, error) {
	// Get job from database
	job, err := db.GetJobByID(database, op.JobID)
	if err != nil {
		return Result{}, fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return Result{}, fmt.Errorf("job %d not found", op.JobID)
	}

	// Skip if job is no longer queued
	if job.Status != db.StatusQueued {
		return Result{
			Success: true,
			JobID:   op.JobID,
			Message: fmt.Sprintf("Job %d already has status '%s', skipping", op.JobID, job.Status),
		}, nil
	}

	// Signal the queue runner to start this job immediately
	queueName := op.QueueName
	if queueName == "" {
		queueName = "default"
	}
	startNowFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.start_now", queueName)
	cmd := fmt.Sprintf("echo %d >> %s", op.JobID, startNowFile)

	_, stderr, err := ssh.RunWithTimeout(job.Host, cmd, opts.Timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return Result{}, fmt.Errorf("connection error: %s", stderr)
		}
		return Result{}, err
	}

	return Result{
		Success: true,
		JobID:   op.JobID,
		Message: fmt.Sprintf("Job %d signaled to start", op.JobID),
	}, nil
}

// executeUpdateQueued updates a queued job's entry in the queue file
func executeUpdateQueued(database *sql.DB, op *db.DeferredOperation, opts ExecuteOptions) (Result, error) {
	// Parse payload
	var payload struct {
		WorkingDir  string   `json:"working_dir"`
		Command     string   `json:"command"`
		Description string   `json:"description"`
		EnvVars     []string `json:"env_vars"`
	}
	if err := json.Unmarshal([]byte(op.Payload), &payload); err != nil {
		return Result{}, fmt.Errorf("parse payload: %w", err)
	}

	queueName := op.QueueName
	if queueName == "" {
		queueName = "default"
	}

	// Build new queue entry line
	envStr := ""
	if len(payload.EnvVars) > 0 {
		for i, ev := range payload.EnvVars {
			if i > 0 {
				envStr += ","
			}
			envStr += ev
		}
	}

	entry := fmt.Sprintf("%d\t%s\t%s\t%s\t%s\t",
		op.JobID, payload.WorkingDir, payload.Command, payload.Description, envStr)

	// Replace entry in queue file
	queueFile := fmt.Sprintf("~/.cache/remote-jobs/queue/%s.queue", queueName)
	updateCmd := fmt.Sprintf("sed -i 's|^%d\t.*|%s|' %s 2>/dev/null || true",
		op.JobID, entry, queueFile)

	_, stderr, err := ssh.RunWithTimeout(op.Host, updateCmd, opts.Timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return Result{}, fmt.Errorf("connection error: %s", stderr)
		}
		return Result{}, err
	}

	return Result{
		Success: true,
		JobID:   op.JobID,
		Message: fmt.Sprintf("Job %d updated in queue", op.JobID),
	}, nil
}

// Helper to get tmux session name
func getTmuxSession(jobID int64, sessionName string) string {
	return session.JobTmuxSession(jobID, sessionName)
}
