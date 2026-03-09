// Package scheduler provides a unified interface for job submission,
// abstracting over local (in-process) and remote (coordinator daemon)
// placement and dispatch.
package scheduler

import (
	"context"
	"database/sql"

	"github.com/osteele/weft/internal/placement"
)

// SubmitRequest describes a job to be placed and dispatched.
type SubmitRequest struct {
	Host        string // Explicit host (empty = auto-place)
	Command     string
	WorkingDir  string
	Description string
	EnvVars     []string
	Tags        []string
	Inputs      []string
	Outputs     []string
	GPUClass    string
	GPUMemGB    int
	DepAfter    int64
	Produces    []string
	Needs       []string
	OutputDirs  []string
	NoSync      bool
}

// SubmitResult reports the outcome of a submission.
type SubmitResult struct {
	JobID           int64
	Host            string // Host the job was placed on (empty = unplaced, needs rental)
	PlacementResult *placement.PlacementResult
}

// Scheduler submits jobs for placement and dispatch.
type Scheduler interface {
	Submit(ctx context.Context, req *SubmitRequest) (*SubmitResult, error)
}

// SelectScheduler returns a RemoteScheduler if the coordinator is reachable,
// otherwise a LocalScheduler for in-process placement and dispatch.
func SelectScheduler(db *sql.DB, coordinatorReachable bool, opts ...Option) Scheduler {
	o := &options{}
	for _, opt := range opts {
		opt(o)
	}
	if coordinatorReachable && o.appConfig != nil {
		return &RemoteScheduler{
			db:        db,
			appConfig: o.appConfig,
		}
	}
	return &LocalScheduler{
		db:        db,
		appConfig: o.appConfig,
		placeFn:   placement.PlaceWithFallback,
	}
}

// Option configures scheduler creation.
type Option func(*options)

type options struct {
	appConfig interface{ GetCoordinatorHost() string }
}

// WithConfig sets the app configuration for the scheduler.
func WithConfig(cfg interface{ GetCoordinatorHost() string }) Option {
	return func(o *options) {
		o.appConfig = cfg
	}
}
