package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/workdir"
)

const ExternalExecutorSkyPilot = "skypilot"

// ExternalJobObservation is one status row observed from an external executor.
type ExternalJobObservation struct {
	Executor                string
	ExternalJobID           string
	ExternalTaskID          string
	ExternalClusterID       string
	ExternalClusterName     string
	RawStatus               string
	RawStatusMessage        string
	NormalizedStatus        string
	SubmittedFromWorkingDir string
	SubmittedFromProject    string
	DashboardURL            string
	Command                 string
	Description             string
	GPUClass                string
	GPUMemGB                *int
	EnvVars                 []string
	Tags                    []string
	SubmittedAt             *int64
	StartedAt               *int64
	EndedAt                 *int64
}

// ExternalJobBinding mirrors a job owned by an external executor.
type ExternalJobBinding struct {
	ID                      int64
	JobID                   int64
	AttemptID               *int64
	Executor                string
	ExternalJobID           string
	ExternalTaskID          string
	ExternalClusterID       string
	ExternalClusterName     string
	RawStatus               string
	RawStatusMessage        string
	NormalizedStatus        string
	SubmittedFromWorkingDir string
	SubmittedFromProject    string
	DashboardURL            string
	CreatedAt               int64
	LastObservedAt          *int64
	// SyncWarning is weft's own record of a failed or inconclusive
	// observation. It is deliberately separate from RawStatusMessage, which
	// belongs to the executor: a transport failure must not overwrite the last
	// thing the executor actually said. See external-executor.allium
	// ExternalObservationLeavesStateAlone.
	SyncWarning   string
	SyncWarningAt *int64
	// CancelRequestedAt is pending external control-plane state. It is shown
	// separately and does not make the authoritative job status terminal.
	CancelRequestedAt *int64
}

const ExternalSubmissionUnconfirmedAfter = 5 * time.Minute

// IsExternalSubmissionUnconfirmed reports when an external job has remained
// nonterminal without a recoverable executor identity past the confirmation
// window. A missing binding before that deadline is still allowed while the
// submit command resolves the executor identity.
func IsExternalSubmissionUnconfirmed(job *Job, binding *ExternalJobBinding, now time.Time) bool {
	if job == nil || binding != nil || job.Backend != BackendSkyPilot || IsTerminalStatus(job.Status) {
		return false
	}
	createdAt := time.Unix(job.CreatedAt, 0)
	return !now.Before(createdAt.Add(ExternalSubmissionUnconfirmedAfter))
}

func normalizeExternalObservation(obs ExternalJobObservation) ExternalJobObservation {
	obs.Executor = strings.TrimSpace(obs.Executor)
	if obs.Executor == "" {
		obs.Executor = ExternalExecutorSkyPilot
	}
	obs.ExternalJobID = strings.TrimSpace(obs.ExternalJobID)
	obs.ExternalTaskID = strings.TrimSpace(obs.ExternalTaskID)
	obs.RawStatus = strings.TrimSpace(obs.RawStatus)
	obs.RawStatusMessage = strings.TrimSpace(obs.RawStatusMessage)
	obs.NormalizedStatus = strings.TrimSpace(obs.NormalizedStatus)
	if obs.NormalizedStatus == "" {
		obs.NormalizedStatus = StatusQueued
	}
	obs.SubmittedFromWorkingDir = strings.TrimSpace(obs.SubmittedFromWorkingDir)
	if obs.SubmittedFromWorkingDir == "" {
		if cwd, err := os.Getwd(); err == nil {
			obs.SubmittedFromWorkingDir = cwd
		}
	}
	if normalized, err := workdir.Normalize(obs.SubmittedFromWorkingDir); err == nil {
		obs.SubmittedFromWorkingDir = normalized
	}
	obs.SubmittedFromProject = strings.TrimSpace(obs.SubmittedFromProject)
	if obs.SubmittedFromProject == "" && obs.SubmittedFromWorkingDir != "" {
		obs.SubmittedFromProject = filepath.Base(obs.SubmittedFromWorkingDir)
	}
	obs.Command = strings.TrimSpace(obs.Command)
	if obs.Command == "" {
		obs.Command = "SkyPilot job " + obs.ExternalJobID
	}
	obs.Description = strings.TrimSpace(obs.Description)
	if obs.Description == "" {
		obs.Description = obs.Command
	}
	return obs
}

func validateConfirmedExternalObservation(obs ExternalJobObservation) error {
	if obs.ExternalJobID == "" {
		return fmt.Errorf("external job id is required")
	}
	if obs.RawStatus == "" {
		return fmt.Errorf("external job status is required for a confirmed observation")
	}
	return nil
}

// UpsertExternalJobFromObservation creates or refreshes a local Weft job for
// an external executor row. It is idempotent on (executor, external_job_id,
// external_task_id), matching the spec's ExternalJobBinding key.
func UpsertExternalJobFromObservation(database *sql.DB, obs ExternalJobObservation) (*ExternalJobBinding, bool, error) {
	obs = normalizeExternalObservation(obs)
	if err := validateConfirmedExternalObservation(obs); err != nil {
		return nil, false, err
	}
	tx, err := database.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	existing, err := findExternalJobBinding(tx, obs.Executor, obs.ExternalJobID, obs.ExternalTaskID)
	if err != nil {
		return nil, false, err
	}
	if existing != nil {
		if _, err := updateExternalJobObservationTx(tx, existing.JobID, obs, now); err != nil {
			return nil, false, err
		}
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		refreshed, err := GetExternalJobBindingByJobID(database, existing.JobID)
		return refreshed, false, err
	}
	if obs.ExternalTaskID != "" {
		unqualified, err := findExternalJobBinding(tx, obs.Executor, obs.ExternalJobID, "")
		if err != nil {
			return nil, false, err
		}
		if unqualified != nil {
			if _, err := updateExternalJobObservationTx(tx, unqualified.JobID, obs, now); err != nil {
				return nil, false, err
			}
			if err := tx.Commit(); err != nil {
				return nil, false, err
			}
			refreshed, err := GetExternalJobBindingByJobID(database, unqualified.JobID)
			return refreshed, false, err
		}
	}

	envVars := encodeStringSlice(obs.EnvVars)
	tags := encodeStringSlice(canonicalizeTagSlice(obs.Tags))
	var gpuMem any
	if obs.GPUMemGB != nil {
		gpuMem = *obs.GPUMemGB
	}
	createdAt := externalTimestampOr(obs.SubmittedAt, now)
	result, err := tx.Exec(`
		INSERT INTO jobs (
			working_dir, command, description, created_at, backend,
			gpu_class, gpu_mem_gb, env_vars, tags, project, placement_host
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
		obs.SubmittedFromWorkingDir, obs.Command, obs.Description, createdAt, BackendSkyPilot,
		obs.GPUClass, gpuMem, envVars, tags, obs.SubmittedFromProject,
	)
	if err != nil {
		return nil, false, fmt.Errorf("create external job: %w", err)
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		return nil, false, err
	}
	attemptID, err := insertExternalAttempt(tx, jobID, obs, now)
	if err != nil {
		return nil, false, err
	}
	if err := upsertExternalBindingTx(tx, jobID, &attemptID, obs, now); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	binding, err := GetExternalJobBindingByJobID(database, jobID)
	return binding, true, err
}

// CreateExternalPendingJob records a local job before the external submission
// is attempted, so the wj<id> remains the canonical address even if submission
// fails.
func CreateExternalPendingJob(database *sql.DB, obs ExternalJobObservation) (int64, int64, error) {
	obs = normalizeExternalObservation(obs)
	tx, err := database.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	var gpuMem any
	if obs.GPUMemGB != nil {
		gpuMem = *obs.GPUMemGB
	}
	result, err := tx.Exec(`
		INSERT INTO jobs (
			working_dir, command, description, created_at, backend,
			gpu_class, gpu_mem_gb, env_vars, tags, project, placement_host
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
		obs.SubmittedFromWorkingDir, obs.Command, obs.Description, now, BackendSkyPilot,
		obs.GPUClass, gpuMem, encodeStringSlice(obs.EnvVars),
		encodeStringSlice(canonicalizeTagSlice(obs.Tags)), obs.SubmittedFromProject,
	)
	if err != nil {
		return 0, 0, fmt.Errorf("create external job: %w", err)
	}
	jobID, err := result.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	attemptID, err := insertExternalAttempt(tx, jobID, obs, now)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return jobID, attemptID, nil
}

func insertExternalAttempt(execer dbExecer, jobID int64, obs ExternalJobObservation, now int64) (int64, error) {
	status := obs.NormalizedStatus
	var startTime any
	if obs.StartedAt != nil && *obs.StartedAt > 0 {
		startTime = *obs.StartedAt
	} else if status == StatusRunning {
		startTime = now
	}
	var endTime any
	// exitCode stays nil for external-executor jobs. SkyPilot's job queue
	// reports a status, not a process exit code (skypilot.Job carries no such
	// field and ParseJobsQueueJSON never populates one), so any value written
	// here would be invented from the status and indistinguishable from an
	// observation. A reader seeing exit_code=1 would conclude the process
	// exited 1, when it may have been OOM-killed at 137 or never have run at
	// all. Terminal outcome is carried by status; the executor's own account
	// is in raw_status / raw_status_message.
	var exitCode any
	if IsTerminalStatus(status) {
		endTime = externalTimestampOr(obs.EndedAt, now)
	}
	queuedAt := externalTimestampOr(obs.SubmittedAt, now)
	result, err := execer.Exec(`
		INSERT INTO job_attempts (
			job_id, attempt_number, host, status, queued_at, start_time,
			end_time, exit_code, backend, remote_id, remote_state,
			last_synced_status, error_message
		) VALUES (?, 1, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, status, queuedAt, startTime, endTime, exitCode, BackendSkyPilot,
		obs.ExternalJobID, obs.RawStatus, status, nullableString(obs.RawStatusMessage),
	)
	if err != nil {
		return 0, fmt.Errorf("create external attempt: %w", err)
	}
	return result.LastInsertId()
}

func upsertExternalBindingTx(execer dbExecer, jobID int64, attemptID *int64, obs ExternalJobObservation, now int64) error {
	var attempt any
	if attemptID != nil {
		attempt = *attemptID
	}
	result, err := execer.Exec(`
		INSERT INTO external_job_bindings (
			job_id, attempt_id, executor, external_job_id, external_task_id,
			external_cluster_id, external_cluster_name, raw_status,
			raw_status_message, normalized_status, submitted_from_working_dir,
			submitted_from_project, dashboard_url, created_at, last_observed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(executor, external_job_id, external_task_id) DO UPDATE SET
			attempt_id = COALESCE(excluded.attempt_id, external_job_bindings.attempt_id),
			external_cluster_id = excluded.external_cluster_id,
			external_cluster_name = excluded.external_cluster_name,
			raw_status = excluded.raw_status,
			raw_status_message = excluded.raw_status_message,
			normalized_status = excluded.normalized_status,
			submitted_from_working_dir = COALESCE(NULLIF(excluded.submitted_from_working_dir, ''), external_job_bindings.submitted_from_working_dir),
			submitted_from_project = COALESCE(NULLIF(excluded.submitted_from_project, ''), external_job_bindings.submitted_from_project),
			dashboard_url = excluded.dashboard_url,
			last_observed_at = excluded.last_observed_at,
			sync_warning = NULL,
			sync_warning_at = NULL
		WHERE external_job_bindings.job_id = excluded.job_id`,
		jobID, attempt, obs.Executor, obs.ExternalJobID, obs.ExternalTaskID,
		obs.ExternalClusterID, obs.ExternalClusterName, obs.RawStatus,
		obs.RawStatusMessage, obs.NormalizedStatus, obs.SubmittedFromWorkingDir,
		obs.SubmittedFromProject, obs.DashboardURL, now, now,
	)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated == 0 {
		return fmt.Errorf("external identity %s/%s/%s is already bound to another job", obs.Executor, obs.ExternalJobID, obs.ExternalTaskID)
	}
	return nil
}

// AttachExternalBinding stores the external ID after a submit that created the
// Weft job first.
func AttachExternalBinding(database *sql.DB, jobID, attemptID int64, obs ExternalJobObservation) error {
	obs = normalizeExternalObservation(obs)
	if err := validateConfirmedExternalObservation(obs); err != nil {
		return err
	}
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	applied, err := updateExternalAttemptTx(tx, jobID, obs, now)
	if err != nil {
		return err
	}
	if !applied {
		return fmt.Errorf("cannot attach external identity to terminal job %d", jobID)
	}
	if err := upsertExternalBindingTx(tx, jobID, &attemptID, obs, now); err != nil {
		return err
	}
	return tx.Commit()
}

// RebindUnconfirmedExternalJob attaches a positively identified external job
// to the original queued SkyPilot job left by an unconfirmed submission.
func RebindUnconfirmedExternalJob(database *sql.DB, jobID int64, obs ExternalJobObservation) (*ExternalJobBinding, error) {
	obs = normalizeExternalObservation(obs)
	if err := validateConfirmedExternalObservation(obs); err != nil {
		return nil, err
	}
	tx, err := database.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var backend string
	if err := tx.QueryRow(`SELECT backend FROM jobs WHERE id = ?`, jobID).Scan(&backend); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("job %d not found", jobID)
		}
		return nil, err
	}
	if backend != BackendSkyPilot {
		return nil, fmt.Errorf("job %d is not a SkyPilot job", jobID)
	}
	var bindingCount int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM external_job_bindings WHERE job_id = ?`, jobID).Scan(&bindingCount); err != nil {
		return nil, err
	}
	if bindingCount != 0 {
		existing, err := getExternalJobBindingByJobID(tx, jobID)
		if err != nil {
			return nil, err
		}
		if bindingCount == 1 && existing != nil && existing.Executor == obs.Executor && existing.ExternalJobID == obs.ExternalJobID {
			switch {
			case existing.ExternalTaskID == obs.ExternalTaskID:
				if _, err := updateExternalJobObservationTx(tx, jobID, obs, time.Now().Unix()); err != nil {
					return nil, err
				}
				if err := tx.Commit(); err != nil {
					return nil, err
				}
				return GetExternalJobBindingByJobID(database, jobID)
			case existing.ExternalTaskID == "" && obs.ExternalTaskID != "":
				attemptID, err := getLatestAttemptIDTx(tx, jobID)
				if err != nil {
					return nil, err
				}
				if attemptID == 0 {
					return nil, fmt.Errorf("job %d has no attempt to refine", jobID)
				}
				var currentStatus string
				if err := tx.QueryRow(`SELECT status FROM job_attempts WHERE id = ?`, attemptID).Scan(&currentStatus); err != nil {
					return nil, err
				}
				if IsTerminalStatus(currentStatus) {
					return nil, fmt.Errorf("job %d is terminal and cannot be rebound", jobID)
				}
				if applied, err := updateExternalJobObservationTx(tx, jobID, obs, time.Now().Unix()); err != nil {
					return nil, err
				} else if !applied {
					return nil, fmt.Errorf("job %d could not refine its external task identity", jobID)
				}
				if err := tx.Commit(); err != nil {
					return nil, err
				}
				return GetExternalJobBindingByJobID(database, jobID)
			}
		}
		return nil, fmt.Errorf("job %d already has a different external binding", jobID)
	}
	attemptID, err := getLatestAttemptIDTx(tx, jobID)
	if err != nil {
		return nil, err
	}
	if attemptID == 0 {
		return nil, fmt.Errorf("job %d has no attempt to rebind", jobID)
	}
	var currentStatus string
	if err := tx.QueryRow(`SELECT status FROM job_attempts WHERE id = ?`, attemptID).Scan(&currentStatus); err != nil {
		return nil, err
	}
	if IsTerminalStatus(currentStatus) {
		return nil, fmt.Errorf("job %d is terminal and cannot be rebound", jobID)
	}
	now := time.Now().Unix()
	applied, err := updateExternalAttemptTx(tx, jobID, obs, now)
	if err != nil {
		return nil, err
	}
	if !applied {
		return nil, fmt.Errorf("job %d could not be rebound", jobID)
	}
	if err := upsertExternalBindingTx(tx, jobID, &attemptID, obs, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return GetExternalJobBindingByJobID(database, jobID)
}

func MarkExternalSubmissionFailed(database *sql.DB, jobID int64, message string) error {
	now := time.Now().Unix()
	// Unfiltered: a submission can fail after its attempt was already closed,
	// so the write must be able to land on a terminal attempt.
	_, err := database.Exec(`
		UPDATE job_attempts
		   SET status = ?, end_time = COALESCE(end_time, ?),
		       error_message = ?, last_synced_status = ?
		 WHERE id = `+latestAttemptSubquery,
		StatusDead, now, message, StatusDead, jobID,
	)
	return err
}

// SetExternalCancelIntent records a successfully sent cancel request on the
// binding only while the latest external attempt is still nonterminal. It
// deliberately does not use jobs.requested_status, which is a terminal status
// overlay rather than a pending external operation.
func SetExternalCancelIntent(database *sql.DB, jobID int64) (bool, error) {
	tx, err := database.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var backend string
	if err := tx.QueryRow(`SELECT backend FROM jobs WHERE id = ?`, jobID).Scan(&backend); err != nil {
		return false, err
	}
	if backend != BackendSkyPilot {
		return false, fmt.Errorf("job %d is not a SkyPilot job", jobID)
	}
	attemptID, err := getLatestAttemptIDTx(tx, jobID)
	if err != nil {
		return false, err
	}
	if attemptID == 0 {
		return false, nil
	}
	var status string
	if err := tx.QueryRow(`SELECT status FROM job_attempts WHERE id = ?`, attemptID).Scan(&status); err != nil {
		return false, err
	}
	if IsTerminalStatus(status) {
		return false, nil
	}
	result, err := tx.Exec(`UPDATE external_job_bindings SET cancel_requested_at = ? WHERE job_id = ?`, time.Now().Unix(), jobID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows != 1 {
		return false, fmt.Errorf("job %d has no unique external binding", jobID)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// MarkExternalSyncWarning records that an external executor refresh could not
// prove fresh state. It deliberately leaves job and attempt status unchanged.
func MarkExternalSyncWarning(database *sql.DB, jobID int64, message string) error {
	_, err := database.Exec(`
		UPDATE external_job_bindings
		   SET sync_warning = ?,
		       sync_warning_at = ?
		 WHERE job_id = ?`,
		strings.TrimSpace(message), time.Now().Unix(), jobID,
	)
	return err
}

// ClearExternalSyncWarning drops a recorded observation failure. A successful
// observation means the channel recovered, so the stale warning must not keep
// implying that weft is out of contact.
func ClearExternalSyncWarning(database dbExecer, jobID int64) error {
	_, err := database.Exec(`
		UPDATE external_job_bindings
		   SET sync_warning = NULL,
		       sync_warning_at = NULL
		 WHERE job_id = ?`, jobID)
	return err
}

func UpdateExternalJobObservation(database *sql.DB, jobID int64, obs ExternalJobObservation) error {
	_, err := ApplyExternalJobObservation(database, jobID, obs)
	return err
}

// ApplyExternalJobObservation returns whether the observation changed the
// mirrored state. A delayed nonterminal snapshot after terminal evidence is a
// successful no-op, not an update.
func ApplyExternalJobObservation(database *sql.DB, jobID int64, obs ExternalJobObservation) (bool, error) {
	obs = normalizeExternalObservation(obs)
	if err := validateConfirmedExternalObservation(obs); err != nil {
		return false, err
	}
	tx, err := database.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	applied, err := updateExternalJobObservationTx(tx, jobID, obs, now)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return applied, nil
}

func updateExternalJobObservationTx(tx *sql.Tx, jobID int64, obs ExternalJobObservation, now int64) (bool, error) {
	existing, err := getExternalJobBindingByJobID(tx, jobID)
	if err != nil {
		return false, err
	}
	refineTaskIdentity := false
	if existing != nil {
		if existing.Executor != obs.Executor || existing.ExternalJobID != obs.ExternalJobID {
			return false, fmt.Errorf("observation identity %s/%s does not match job %d binding %s/%s", obs.Executor, obs.ExternalJobID, jobID, existing.Executor, existing.ExternalJobID)
		}
		switch {
		case existing.ExternalTaskID == obs.ExternalTaskID:
		case existing.ExternalTaskID == "" && obs.ExternalTaskID != "":
			refineTaskIdentity = true
		default:
			return false, fmt.Errorf("observation task %q does not match job %d binding task %q", obs.ExternalTaskID, jobID, existing.ExternalTaskID)
		}
	}
	applied, err := updateExternalAttemptTx(tx, jobID, obs, now)
	if err != nil {
		return false, err
	}
	if !applied {
		return false, nil
	}
	if IsTerminalStatus(obs.NormalizedStatus) {
		if _, err := tx.Exec(`UPDATE jobs SET requested_status = NULL WHERE id = ?`, jobID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(`UPDATE external_job_bindings SET cancel_requested_at = NULL WHERE job_id = ?`, jobID); err != nil {
			return false, err
		}
	}
	if refineTaskIdentity {
		result, err := tx.Exec(`
			UPDATE external_job_bindings
			   SET external_task_id = ?
			 WHERE id = ? AND job_id = ? AND external_task_id = ''`,
			obs.ExternalTaskID, existing.ID, jobID)
		if err != nil {
			return false, err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return false, err
		}
		if rows != 1 {
			return false, fmt.Errorf("external task identity refinement lost a concurrent update for job %d", jobID)
		}
	}
	attemptID, err := getLatestAttemptIDTx(tx, jobID)
	if err != nil {
		return false, err
	}
	var attemptPtr *int64
	if attemptID > 0 {
		attemptPtr = &attemptID
	}
	if err := upsertExternalBindingTx(tx, jobID, attemptPtr, obs, now); err != nil {
		return false, err
	}
	return true, nil
}

func updateExternalAttemptTx(execer dbExecer, jobID int64, obs ExternalJobObservation, now int64) (bool, error) {
	attemptID, err := getLatestAttemptIDTx(execer, jobID)
	if err != nil {
		return false, err
	}
	if attemptID == 0 {
		_, err := insertExternalAttempt(execer, jobID, obs, now)
		return err == nil, err
	}
	status := obs.NormalizedStatus
	var currentStatus, currentRawStatus string
	if err := execer.QueryRow(`SELECT status, COALESCE(remote_state, '') FROM job_attempts WHERE id = ?`, attemptID).Scan(&currentStatus, &currentRawStatus); err != nil {
		return false, err
	}
	if IsTerminalStatus(currentStatus) &&
		(currentStatus != status ||
			(IsTerminalStatus(status) && !strings.EqualFold(strings.TrimSpace(currentRawStatus), strings.TrimSpace(obs.RawStatus)))) {
		// External terminal states are final. Without a source timestamp, a
		// later DB writer cannot prove that a conflicting queue snapshot is a
		// newer executor result. This includes two executor states that normalize
		// to the same local bucket: neither may rewrite the first raw outcome.
		return false, nil
	}
	// See insertExternalAttempt: exit codes are not observable from SkyPilot,
	// so none is synthesised here either.
	var exitCode any
	terminal := IsTerminalStatus(status)
	var submittedAt any
	if obs.SubmittedAt != nil && *obs.SubmittedAt > 0 {
		submittedAt = *obs.SubmittedAt
	}
	var startedAt any
	if obs.StartedAt != nil && *obs.StartedAt > 0 {
		startedAt = *obs.StartedAt
	} else if status == StatusRunning {
		startedAt = now
	}
	endAt := externalTimestampOr(obs.EndedAt, now)
	// No internal-table guard by design: the external queue (SkyPilot) is
	// authoritative for its own lifecycle, so the written status is the
	// executor's normalized observation, not an internal transition. The
	// terminal-finality check above is the guard — it refuses to rewrite a
	// terminal attempt with a conflicting outcome.
	_, err = execer.Exec(`
		UPDATE job_attempts
		   SET status = ?,
		       queued_at = CASE WHEN ? IS NOT NULL THEN ? ELSE queued_at END,
		       start_time = CASE
		           WHEN ? IS NOT NULL AND (start_time IS NULL OR start_time = 0) THEN ?
		           ELSE start_time
		       END,
		       end_time = CASE
		           WHEN ? THEN COALESCE(NULLIF(end_time, 0), ?)
		           ELSE NULL
		       END,
		       exit_code = ?,
		       backend = ?,
		       remote_id = ?,
		       remote_state = ?,
		       last_synced_status = ?,
		       error_message = ?
		 WHERE id = ?`,
		status, submittedAt, submittedAt, startedAt, startedAt, terminal, endAt, exitCode, BackendSkyPilot,
		obs.ExternalJobID, obs.RawStatus, status, nullableString(obs.RawStatusMessage), attemptID,
	)
	return err == nil, err
}

func externalTimestampOr(value *int64, fallback int64) int64 {
	if value != nil && *value > 0 {
		return *value
	}
	return fallback
}

func getLatestAttemptIDTx(execer dbExecer, jobID int64) (int64, error) {
	var id sql.NullInt64
	err := execer.QueryRow(`SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`, jobID).Scan(&id)
	if err == sql.ErrNoRows || !id.Valid {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id.Int64, nil
}

func nullableString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func canonicalizeTagSlice(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	result := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		result = append(result, CanonicalizeTag(tag))
	}
	return result
}

func FindExternalJobBinding(database *sql.DB, executor, externalJobID, externalTaskID string) (*ExternalJobBinding, error) {
	return findExternalJobBinding(database, executor, externalJobID, externalTaskID)
}

func findExternalJobBinding(execer dbExecer, executor, externalJobID, externalTaskID string) (*ExternalJobBinding, error) {
	row := execer.QueryRow(`
		SELECT id, job_id, attempt_id, executor, external_job_id, external_task_id,
		       external_cluster_id, external_cluster_name, raw_status,
		       raw_status_message, normalized_status, submitted_from_working_dir,
		       submitted_from_project, dashboard_url, created_at, last_observed_at,
		       sync_warning, sync_warning_at, cancel_requested_at
		  FROM external_job_bindings
		 WHERE executor = ? AND external_job_id = ? AND external_task_id = ?`,
		executor, externalJobID, externalTaskID,
	)
	return scanExternalJobBinding(row)
}

func GetExternalJobBindingByJobID(database *sql.DB, jobID int64) (*ExternalJobBinding, error) {
	return getExternalJobBindingByJobID(database, jobID)
}

func getExternalJobBindingByJobID(execer dbExecer, jobID int64) (*ExternalJobBinding, error) {
	row := execer.QueryRow(`
		SELECT id, job_id, attempt_id, executor, external_job_id, external_task_id,
		       external_cluster_id, external_cluster_name, raw_status,
		       raw_status_message, normalized_status, submitted_from_working_dir,
		       submitted_from_project, dashboard_url, created_at, last_observed_at,
		       sync_warning, sync_warning_at, cancel_requested_at
		  FROM external_job_bindings
		 WHERE job_id = ?
		 ORDER BY id DESC LIMIT 1`, jobID)
	return scanExternalJobBinding(row)
}

func ListExternalJobBindings(database *sql.DB, executor, project string) ([]*ExternalJobBinding, error) {
	query := `
		SELECT b.id, b.job_id, b.attempt_id, b.executor, b.external_job_id, b.external_task_id,
		       b.external_cluster_id, b.external_cluster_name, b.raw_status,
		       b.raw_status_message, b.normalized_status, b.submitted_from_working_dir,
		       b.submitted_from_project, b.dashboard_url, b.created_at, b.last_observed_at,
		       b.sync_warning, b.sync_warning_at, b.cancel_requested_at
		  FROM external_job_bindings b
		  JOIN jobs j ON j.id = b.job_id
		 WHERE b.executor = ? AND j.tombstoned = 0`
	args := []any{executor}
	if strings.TrimSpace(project) != "" {
		query += ` AND j.project = ?`
		args = append(args, strings.TrimSpace(project))
	}
	query += ` ORDER BY b.job_id ASC`
	rows, err := database.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []*ExternalJobBinding
	for rows.Next() {
		b, err := scanExternalJobBindingRows(rows)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, b)
	}
	return bindings, rows.Err()
}

type externalBindingScanner interface {
	Scan(dest ...any) error
}

func scanExternalJobBinding(row externalBindingScanner) (*ExternalJobBinding, error) {
	b, err := scanExternalJobBindingRows(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return b, err
}

func scanExternalJobBindingRows(row externalBindingScanner) (*ExternalJobBinding, error) {
	var b ExternalJobBinding
	var attemptID, lastObserved, syncWarningAt, cancelRequestedAt sql.NullInt64
	var clusterID, clusterName, rawStatus, rawMessage, normalized, wd, project, dashboard, syncWarning sql.NullString
	err := row.Scan(&b.ID, &b.JobID, &attemptID, &b.Executor, &b.ExternalJobID, &b.ExternalTaskID,
		&clusterID, &clusterName, &rawStatus, &rawMessage, &normalized, &wd, &project,
		&dashboard, &b.CreatedAt, &lastObserved, &syncWarning, &syncWarningAt, &cancelRequestedAt)
	if err != nil {
		return nil, err
	}
	if attemptID.Valid {
		b.AttemptID = &attemptID.Int64
	}
	if syncWarning.Valid {
		b.SyncWarning = syncWarning.String
	}
	if syncWarningAt.Valid {
		b.SyncWarningAt = &syncWarningAt.Int64
	}
	if cancelRequestedAt.Valid {
		b.CancelRequestedAt = &cancelRequestedAt.Int64
	}
	if clusterID.Valid {
		b.ExternalClusterID = clusterID.String
	}
	if clusterName.Valid {
		b.ExternalClusterName = clusterName.String
	}
	if rawStatus.Valid {
		b.RawStatus = rawStatus.String
	}
	if rawMessage.Valid {
		b.RawStatusMessage = rawMessage.String
	}
	if normalized.Valid {
		b.NormalizedStatus = normalized.String
	}
	if wd.Valid {
		b.SubmittedFromWorkingDir = wd.String
	}
	if project.Valid {
		b.SubmittedFromProject = project.String
	}
	if dashboard.Valid {
		b.DashboardURL = dashboard.String
	}
	if lastObserved.Valid {
		b.LastObservedAt = &lastObserved.Int64
	}
	return &b, nil
}
