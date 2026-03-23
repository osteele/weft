package db

import (
	"database/sql"
	"fmt"
)

// TrainingJobRun is a stable run-based record for predictor training and export.
type TrainingJobRun struct {
	RunID          int64
	JobID          int64
	Host           string
	WorkingDir     string
	Command        string
	Project        string
	GPUClass       string
	Backend        string
	Tenant         string
	StartTime      int64
	EndTime        int64
	DurationS      int64
	ExitCode       int
	FailureReason  string
	ErrorDiagnosis string
	Metadata       *JobMetadata
}

// ListTrainingJobRuns returns terminal execution attempts ordered by start time.
func ListTrainingJobRuns(db *sql.DB, sinceUnix int64) ([]TrainingJobRun, error) {
	query := `
		SELECT run_id, job_id, host, working_dir, command, project, gpu_class, backend, tenant,
		       start_time, end_time, COALESCE(duration_s, 0), COALESCE(exit_code, 0),
		       failure_reason, error_diagnosis, job_metadata
		FROM job_run_training_examples
	`
	var args []any
	if sinceUnix > 0 {
		query += ` WHERE start_time >= ?`
		args = append(args, sinceUnix)
	}
	query += ` ORDER BY start_time ASC, run_id ASC`

	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("query training job runs: %w", err)
	}
	defer rows.Close()

	var runs []TrainingJobRun
	for rows.Next() {
		var run TrainingJobRun
		var workingDir, project, gpuClass, backend, tenant sql.NullString
		var failureReason, errorDiagnosis, jobMetadata sql.NullString
		if err := rows.Scan(
			&run.RunID, &run.JobID, &run.Host, &workingDir, &run.Command, &project, &gpuClass, &backend, &tenant,
			&run.StartTime, &run.EndTime, &run.DurationS, &run.ExitCode,
			&failureReason, &errorDiagnosis, &jobMetadata,
		); err != nil {
			return nil, fmt.Errorf("scan training job run: %w", err)
		}
		if workingDir.Valid {
			run.WorkingDir = workingDir.String
		}
		if project.Valid {
			run.Project = project.String
		}
		if gpuClass.Valid {
			run.GPUClass = gpuClass.String
		}
		if backend.Valid {
			run.Backend = backend.String
		} else {
			run.Backend = BackendQueueRunner
		}
		if tenant.Valid {
			run.Tenant = tenant.String
		}
		if failureReason.Valid {
			run.FailureReason = failureReason.String
		}
		if errorDiagnosis.Valid {
			run.ErrorDiagnosis = errorDiagnosis.String
		}
		run.Metadata = decodeJobMetadata(jobMetadata)
		runs = append(runs, run)
	}
	return runs, rows.Err()
}
