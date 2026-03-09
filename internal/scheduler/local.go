package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/workdir"
)

// PlaceFunc is the signature for host placement. See placement.PlaceWithFallback.
type PlaceFunc func(db *sql.DB, constraints placement.Constraints, predict placement.JobPredictor) (*placement.PlacementResult, error)

// LocalScheduler performs placement and dispatch in-process, without
// requiring a remote coordinator daemon.
type LocalScheduler struct {
	db        *sql.DB
	appConfig interface{ GetCoordinatorHost() string }
	placeFn   PlaceFunc
}

// Submit places the job on the best available host and queues it directly.
func (s *LocalScheduler) Submit(_ context.Context, req *SubmitRequest) (*SubmitResult, error) {
	constraints := placement.Constraints{
		GPUClass: req.GPUClass,
		GPUMemGB: req.GPUMemGB,
		Inputs:   req.Inputs,
		Command:  req.Command,
		Project:  workdir.ProjectName(req.WorkingDir),
		Tags:     req.Tags,
	}

	var cfg *config.Config
	if c, ok := s.appConfig.(*config.Config); ok {
		cfg = c
	}
	predict := placement.BuildJobPredictorFromConfig(cfg, constraints)

	host := req.Host
	var placementResult *placement.PlacementResult

	if host == "" {
		result, err := s.placeFn(s.db, constraints, predict)
		if err != nil {
			if !errors.Is(err, placement.ErrNoEligibleHost) {
				return nil, err
			}
			// No eligible host — job will be created as unplaced (host="")
		} else {
			placementResult = result
			host = result.Host

			oplog.Log(oplog.OpPlacementDecided,
				oplog.WithHost(host),
				oplog.WithDetail(placement.FormatPlacementDetail(result)))
		}
	}

	// Build queue params
	gpuMemPtr := (*int)(nil)
	if req.GPUMemGB > 0 {
		mem := req.GPUMemGB
		gpuMemPtr = &mem
	}
	depSpec := ""
	if req.DepAfter > 0 {
		depSpec = fmt.Sprintf("%d", req.DepAfter)
	}

	params := ops.QueueJobParams{
		Host:        host,
		WorkingDir:  req.WorkingDir,
		Command:     req.Command,
		Description: req.Description,
		EnvVars:     req.EnvVars,
		Tags:        req.Tags,
		GPUClass:    req.GPUClass,
		GPUMemGB:    gpuMemPtr,
		DepSpec:     depSpec,
		Inputs:      req.Inputs,
		Outputs:     req.Outputs,
		OutputDirs:  req.OutputDirs,
		Produces:    req.Produces,
		Needs:       req.Needs,
	}

	jobID, err := ops.RecordQueuedJob(s.db, params)
	if err != nil {
		return nil, err
	}

	return &SubmitResult{
		JobID:           jobID,
		Host:            host,
		PlacementResult: placementResult,
	}, nil
}
