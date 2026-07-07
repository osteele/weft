package daemonapi

import (
	"context"
	"database/sql"
	"fmt"
)

const defaultWriteExecutorQueueSize = 256

// WriteExecutor serializes local DB mutations through one daemon-owned worker.
// It keeps socket handlers from competing with each other for SQLite's single
// writer slot and gives the daemon one place to route future background writes.
type WriteExecutor struct {
	database *sql.DB
	jobs     chan writeExecutorJob
}

type writeExecutorJob struct {
	ctx    context.Context
	fn     func(context.Context, *sql.DB) (any, error)
	result chan writeExecutorResult
}

type writeExecutorResult struct {
	value any
	err   error
}

func NewWriteExecutor(ctx context.Context, database *sql.DB) *WriteExecutor {
	if ctx == nil {
		ctx = context.Background()
	}
	exec := &WriteExecutor{
		database: database,
		jobs:     make(chan writeExecutorJob, defaultWriteExecutorQueueSize),
	}
	go exec.run(ctx)
	return exec
}

func (e *WriteExecutor) Execute(ctx context.Context, fn func(context.Context, *sql.DB) (any, error)) (any, error) {
	if e == nil {
		return nil, fmt.Errorf("write executor is nil")
	}
	if fn == nil {
		return nil, fmt.Errorf("write executor requires a function")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	job := writeExecutorJob{
		ctx:    ctx,
		fn:     fn,
		result: make(chan writeExecutorResult, 1),
	}
	select {
	case e.jobs <- job:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case result := <-job.result:
		return result.value, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *WriteExecutor) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-e.jobs:
			e.runJob(job)
		}
	}
}

func (e *WriteExecutor) runJob(job writeExecutorJob) {
	if err := job.ctx.Err(); err != nil {
		job.result <- writeExecutorResult{err: err}
		return
	}
	value, err := job.fn(job.ctx, e.database)
	job.result <- writeExecutorResult{value: value, err: err}
}
