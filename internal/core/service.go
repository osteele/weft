package core

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/coordinatorrelay"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
)

// Service owns job orchestration for both CLI and TUI facades. It encapsulates
// database access so higher layers don't need to manage SQL handles for write
// operations.
type Service struct {
	database *sql.DB
	ownsDB   bool
	cfg      *config.Config
	relay    *coordinatorrelay.Client
}

// OperationResult captures the outcome of executing a core operation.
type OperationResult struct {
	Job     *db.Job
	Outcome ops.Result
}

// NewService opens the job database and returns a Service that owns the handle.
func NewService() (*Service, error) {
	database, err := db.Open()
	if err != nil {
		return nil, err
	}
	cfg, _ := config.Load()
	relay, err := coordinatorrelay.NewClientFromConfig(cfg)
	if err != nil {
		database.Close()
		return nil, err
	}
	return &Service{database: database, ownsDB: true, cfg: cfg, relay: relay}, nil
}

// NewServiceWithDB wraps an existing database handle. Callers remain
// responsible for closing the database.
func NewServiceWithDB(database *sql.DB) *Service {
	cfg, _ := config.Load()
	relay, _ := coordinatorrelay.NewClientFromConfig(cfg)
	return &Service{database: database, ownsDB: false, cfg: cfg, relay: relay}
}

// Close releases owned resources. It only closes the underlying DB when the
// service created it via NewService.
func (s *Service) Close() error {
	if s == nil || !s.ownsDB || s.database == nil {
		return nil
	}
	err := s.database.Close()
	s.database = nil
	return err
}

// Database returns the underlying database handle.
func (s *Service) Database() *sql.DB {
	return s.database
}

// Job loads a job by ID.
func (s *Service) Job(jobID int64) (*db.Job, error) {
	return s.loadJob(jobID)
}

// KillJob terminates running jobs or cancels queued ones. Timeout mode controls
// how long the helper waits for reconciliation before deferring.
func (s *Service) KillJob(jobID int64, mode ops.TimeoutMode) (OperationResult, error) {
	job, err := s.loadJob(jobID)
	if err != nil {
		return OperationResult{}, err
	}

	opts := ops.OptionsForMode(resolveMode(mode))
	var outcome ops.Result
	effectiveStatus := job.EffectiveStatus()
	switch effectiveStatus {
	case db.StatusQueued:
		if s.shouldRelay(job) {
			return s.relayQueuedCancel(job)
		}
		outcome, err = ops.CancelQueuedJob(s.database, job, opts)
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		if s.shouldRelay(job) {
			return s.relayStatus(job, coordinatorrelay.OpKillJob, db.StatusKilled)
		}
		outcome, err = ops.KillJob(s.database, job, opts)
	default:
		return OperationResult{}, fmt.Errorf("job %d is %s; nothing to kill", job.ID, effectiveStatus)
	}
	if err != nil {
		return OperationResult{}, err
	}
	return s.operationResult(jobID, outcome)
}

// DraftJob marks a job draft and cleans up any remote execution.
func (s *Service) DraftJob(jobID int64, mode ops.TimeoutMode) (OperationResult, error) {
	job, err := s.loadJob(jobID)
	if err != nil {
		return OperationResult{}, err
	}
	outcome, err := ops.DraftJob(s.database, job, ops.OptionsForMode(resolveMode(mode)))
	if err != nil {
		return OperationResult{}, err
	}
	return s.operationResult(jobID, outcome)
}

// PauseJob pauses a running job (SIGSTOP) so it can be resumed later.
func (s *Service) PauseJob(jobID int64, mode ops.TimeoutMode) (OperationResult, error) {
	job, err := s.loadJob(jobID)
	if err != nil {
		return OperationResult{}, err
	}
	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus == db.StatusPaused {
		return OperationResult{}, fmt.Errorf("job %d is already paused", job.ID)
	}
	if effectiveStatus != db.StatusRunning && effectiveStatus != db.StatusStarting {
		return OperationResult{}, fmt.Errorf("job %d is %s; only running jobs can be paused", job.ID, effectiveStatus)
	}
	if s.shouldRelay(job) {
		return s.relayStatus(job, coordinatorrelay.OpPauseJob, db.StatusPaused)
	}
	outcome, err := ops.RequestStatus(s.database, job, db.StatusPaused, resolveMode(mode))
	if err != nil {
		return OperationResult{}, err
	}
	return s.operationResult(jobID, outcome)
}

// ResumeJob resumes a paused job (SIGCONT).
func (s *Service) ResumeJob(jobID int64, mode ops.TimeoutMode) (OperationResult, error) {
	job, err := s.loadJob(jobID)
	if err != nil {
		return OperationResult{}, err
	}
	effectiveStatus := job.EffectiveStatus()
	if effectiveStatus != db.StatusPaused {
		return OperationResult{}, fmt.Errorf("job %d is %s; only paused jobs can be resumed", job.ID, effectiveStatus)
	}
	if s.shouldRelay(job) {
		return s.relayStatus(job, coordinatorrelay.OpResumeJob, db.StatusRunning)
	}
	outcome, err := ops.RequestStatus(s.database, job, db.StatusRunning, resolveMode(mode))
	if err != nil {
		return OperationResult{}, err
	}
	return s.operationResult(jobID, outcome)
}

// RequestStatus updates the target status and immediately attempts
// reconciliation using the provided timeout mode.
func (s *Service) RequestStatus(jobID int64, targetStatus string, mode ops.TimeoutMode) (OperationResult, error) {
	job, err := s.loadJob(jobID)
	if err != nil {
		return OperationResult{}, err
	}
	outcome, err := ops.RequestStatus(s.database, job, targetStatus, resolveMode(mode))
	if err != nil {
		return OperationResult{}, err
	}
	return s.operationResult(jobID, outcome)
}

func (s *Service) loadJob(jobID int64) (*db.Job, error) {
	if s == nil || s.database == nil {
		return nil, fmt.Errorf("core service unavailable")
	}
	if jobID <= 0 {
		return nil, fmt.Errorf("invalid job ID %d", jobID)
	}
	job, err := db.GetJobByID(s.database, jobID)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, fmt.Errorf("job %d not found", jobID)
	}
	return job, nil
}

func (s *Service) operationResult(jobID int64, outcome ops.Result) (OperationResult, error) {
	var job *db.Job
	if s.database != nil {
		var err error
		job, err = db.GetJobByID(s.database, jobID)
		if err != nil {
			return OperationResult{}, err
		}
	}
	return OperationResult{
		Job:     job,
		Outcome: outcome,
	}, nil
}

func resolveMode(mode ops.TimeoutMode) ops.TimeoutMode {
	if mode == ops.TimeoutFast || mode == ops.TimeoutNormal || mode == ops.TimeoutSync {
		return mode
	}
	return ops.TimeoutNormal
}

func (s *Service) shouldRelay(job *db.Job) bool {
	return s != nil && s.relay != nil && job != nil && !job.IsLaunchJob()
}

func (s *Service) relayQueuedCancel(job *db.Job) (OperationResult, error) {
	if err := db.SetPendingStatus(s.database, job.ID, db.StatusCanceled); err != nil {
		return OperationResult{}, err
	}
	return s.sendRelay(job, coordinatorrelay.OpCancelJob, "cancel requested")
}

func (s *Service) relayStatus(job *db.Job, op string, targetStatus string) (OperationResult, error) {
	if err := db.SetPendingStatus(s.database, job.ID, targetStatus); err != nil {
		return OperationResult{}, err
	}
	return s.sendRelay(job, op, targetStatus+" requested")
}

func (s *Service) sendRelay(job *db.Job, op string, fallbackMessage string) (OperationResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), coordinatorrelay.DefaultAckWait)
	defer cancel()
	ack, err := s.relay.Submit(ctx, &coordinatorrelay.Request{
		Op:    op,
		JobID: job.ID,
	})
	if err != nil {
		return OperationResult{}, err
	}
	if !ack.Accepted {
		return OperationResult{}, fmt.Errorf("%s", ack.Message)
	}
	if ack.Message == "" {
		ack.Message = fallbackMessage
	}
	return s.operationResult(job.ID, ops.Result{
		Success: true,
		JobID:   job.ID,
		Message: ack.Message,
	})
}
