package services

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/remediation"
	"github.com/osteele/weft/internal/ssh"
)

// Remediator periodically scans for failed jobs, diagnoses errors, and
// attempts auto-remediation when possible.
type Remediator struct {
	database  *sql.DB
	logger    *slog.Logger
	appConfig *config.Config
	interval  time.Duration
}

// NewRemediator creates a new remediator service.
func NewRemediator(database *sql.DB, logger *slog.Logger, appConfig *config.Config, interval time.Duration) *Remediator {
	return &Remediator{
		database:  database,
		logger:    logger,
		appConfig: appConfig,
		interval:  interval,
	}
}

// Start runs the remediator loop until the context is cancelled.
func (r *Remediator) Start(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.CheckFailedJobs()
		}
	}
}

// CheckFailedJobs scans for recently failed jobs and attempts diagnosis/remediation.
func (r *Remediator) CheckFailedJobs() int {
	jobs, err := db.ListRecentFailedUndiagnosed(r.database, 10)
	if err != nil {
		r.logger.Warn("failed to list failed jobs for remediation", "error", err)
		return 0
	}
	if len(jobs) == 0 {
		return 0
	}

	type jobLog struct {
		job        *db.Job
		logContent string
	}
	results := make(chan jobLog, len(jobs))
	sem := make(chan struct{}, 4)

	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(j *db.Job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			logContent := fetchJobLog(j)
			results <- jobLog{job: j, logContent: logContent}
		}(job)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	processed := 0
	for jl := range results {
		if jl.logContent == "" {
			r.logger.Debug("no log content, skipping diagnosis", "job_id", jl.job.ID, "host", jl.job.Host)
			continue
		}

		ctx := remediation.RemediationContext{
			DB:         r.database,
			Job:        jl.job,
			LogContent: jl.logContent,
			Logger:     r.logger,
			Config:     r.appConfig,
		}

		result := remediation.AttemptRemediation(ctx)
		if result == nil {
			continue
		}
		processed++

		oplog.LogJob(oplog.OpCoordinatorDiagnosis, jl.job.ID, jl.job.Host,
			oplog.WithDetailf("pattern=%s category=%s", result.Diagnosis.Pattern, result.Diagnosis.Category))

		if result.Retried {
			r.logger.Info("remediated job", "job_id", jl.job.ID, "action", result.Action)
			oplog.LogJob(oplog.OpCoordinatorRemediation, jl.job.ID, jl.job.Host,
				oplog.WithDetailf("action=%s", result.Action))
		} else {
			r.logger.Info("diagnosed job", "job_id", jl.job.ID, "message", result.Diagnosis.Message, "action", result.Action)
		}
	}
	return processed
}

// fetchJobLog retrieves the last 200 lines of a job's log from the remote host.
func fetchJobLog(job *db.Job) string {
	cmd := fmt.Sprintf("tail -200 ~/.cache/weft/logs/%d-*.log 2>/dev/null", job.ID)
	stdout, _, err := ssh.RunWithTimeout(job.Host, cmd, 10*time.Second)
	if err != nil {
		return ""
	}
	return stdout
}
