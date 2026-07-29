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

// UpsertExternalJobFromObservation creates or refreshes a local Weft job for
// an external executor row. It is idempotent on (executor, external_job_id,
// external_task_id), matching the spec's ExternalJobBinding key.
func UpsertExternalJobFromObservation(database *sql.DB, obs ExternalJobObservation) (*ExternalJobBinding, bool, error) {
	obs = normalizeExternalObservation(obs)
	if obs.ExternalJobID == "" {
		return nil, false, fmt.Errorf("external job id is required")
	}
	if existing, err := FindExternalJobBinding(database, obs.Executor, obs.ExternalJobID, obs.ExternalTaskID); err != nil {
		return nil, false, err
	} else if existing != nil {
		if err := UpdateExternalJobObservation(database, existing.JobID, obs); err != nil {
			return nil, false, err
		}
		refreshed, err := GetExternalJobBindingByJobID(database, existing.JobID)
		return refreshed, false, err
	}

	tx, err := database.Begin()
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	envVars := encodeStringSlice(obs.EnvVars)
	tags := encodeStringSlice(canonicalizeTagSlice(obs.Tags))
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
	if status == StatusRunning {
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
		endTime = now
	}
	result, err := execer.Exec(`
		INSERT INTO job_attempts (
			job_id, attempt_number, host, status, queued_at, start_time,
			end_time, exit_code, backend, remote_id, remote_state,
			last_synced_status, error_message
		) VALUES (?, 1, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, status, now, startTime, endTime, exitCode, BackendSkyPilot,
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
	_, err := execer.Exec(`
		INSERT INTO external_job_bindings (
			job_id, attempt_id, executor, external_job_id, external_task_id,
			external_cluster_id, external_cluster_name, raw_status,
			raw_status_message, normalized_status, submitted_from_working_dir,
			submitted_from_project, dashboard_url, created_at, last_observed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(executor, external_job_id, external_task_id) DO UPDATE SET
			job_id = excluded.job_id,
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
			sync_warning_at = NULL`,
		jobID, attempt, obs.Executor, obs.ExternalJobID, obs.ExternalTaskID,
		obs.ExternalClusterID, obs.ExternalClusterName, obs.RawStatus,
		obs.RawStatusMessage, obs.NormalizedStatus, obs.SubmittedFromWorkingDir,
		obs.SubmittedFromProject, obs.DashboardURL, now, now,
	)
	return err
}

// AttachExternalBinding stores the external ID after a submit that created the
// Weft job first.
func AttachExternalBinding(database *sql.DB, jobID, attemptID int64, obs ExternalJobObservation) error {
	obs = normalizeExternalObservation(obs)
	if obs.ExternalJobID == "" {
		return fmt.Errorf("external job id is required")
	}
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if err := upsertExternalBindingTx(tx, jobID, &attemptID, obs, now); err != nil {
		return err
	}
	if err := updateExternalAttemptTx(tx, jobID, obs, now); err != nil {
		return err
	}
	return tx.Commit()
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
	obs = normalizeExternalObservation(obs)
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if err := updateExternalAttemptTx(tx, jobID, obs, now); err != nil {
		return err
	}
	attemptID, err := getLatestAttemptIDTx(tx, jobID)
	if err != nil {
		return err
	}
	var attemptPtr *int64
	if attemptID > 0 {
		attemptPtr = &attemptID
	}
	if err := upsertExternalBindingTx(tx, jobID, attemptPtr, obs, now); err != nil {
		return err
	}
	return tx.Commit()
}

func updateExternalAttemptTx(execer dbExecer, jobID int64, obs ExternalJobObservation, now int64) error {
	attemptID, err := getLatestAttemptIDTx(execer, jobID)
	if err != nil {
		return err
	}
	if attemptID == 0 {
		_, err := insertExternalAttempt(execer, jobID, obs, now)
		return err
	}
	status := obs.NormalizedStatus
	var endTime any
	// See insertExternalAttempt: exit codes are not observable from SkyPilot,
	// so none is synthesised here either.
	var exitCode any
	if IsTerminalStatus(status) {
		endTime = now
	}
	_, err = execer.Exec(`
		UPDATE job_attempts
		   SET status = ?,
		       start_time = CASE
		           WHEN ? = ? AND (start_time IS NULL OR start_time = 0) THEN ?
		           ELSE start_time
		       END,
		       end_time = ?,
		       exit_code = ?,
		       backend = ?,
		       remote_id = ?,
		       remote_state = ?,
		       last_synced_status = ?,
		       error_message = ?
		 WHERE id = ?`,
		status, status, StatusRunning, now, endTime, exitCode, BackendSkyPilot,
		obs.ExternalJobID, obs.RawStatus, status, nullableString(obs.RawStatusMessage), attemptID,
	)
	return err
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
	row := database.QueryRow(`
		SELECT id, job_id, attempt_id, executor, external_job_id, external_task_id,
		       external_cluster_id, external_cluster_name, raw_status,
		       raw_status_message, normalized_status, submitted_from_working_dir,
		       submitted_from_project, dashboard_url, created_at, last_observed_at,
		       sync_warning, sync_warning_at
		  FROM external_job_bindings
		 WHERE executor = ? AND external_job_id = ? AND external_task_id = ?`,
		executor, externalJobID, externalTaskID,
	)
	return scanExternalJobBinding(row)
}

func GetExternalJobBindingByJobID(database *sql.DB, jobID int64) (*ExternalJobBinding, error) {
	row := database.QueryRow(`
		SELECT id, job_id, attempt_id, executor, external_job_id, external_task_id,
		       external_cluster_id, external_cluster_name, raw_status,
		       raw_status_message, normalized_status, submitted_from_working_dir,
		       submitted_from_project, dashboard_url, created_at, last_observed_at,
		       sync_warning, sync_warning_at
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
		       b.sync_warning, b.sync_warning_at
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
	var attemptID, lastObserved, syncWarningAt sql.NullInt64
	var clusterID, clusterName, rawStatus, rawMessage, normalized, wd, project, dashboard, syncWarning sql.NullString
	err := row.Scan(&b.ID, &b.JobID, &attemptID, &b.Executor, &b.ExternalJobID, &b.ExternalTaskID,
		&clusterID, &clusterName, &rawStatus, &rawMessage, &normalized, &wd, &project,
		&dashboard, &b.CreatedAt, &lastObserved, &syncWarning, &syncWarningAt)
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
