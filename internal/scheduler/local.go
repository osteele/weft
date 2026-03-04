package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
)

// LocalScheduler performs placement and dispatch in-process, without
// requiring a remote coordinator daemon.
type LocalScheduler struct {
	db        *sql.DB
	appConfig interface{ GetCoordinatorHost() string }
}

// Submit places the job on the best available host and queues it directly.
func (s *LocalScheduler) Submit(_ context.Context, req *SubmitRequest) (*SubmitResult, error) {
	constraints := placement.Constraints{
		GPUClass: req.GPUClass,
		GPUMemGB: req.GPUMemGB,
		Inputs:   req.Inputs,
		Command:  req.Command,
		Project:  workdir.ProjectName(req.WorkingDir),
	}

	var cfg *config.Config
	if c, ok := s.appConfig.(*config.Config); ok {
		cfg = c
	}
	predict := placement.BuildJobPredictorFromConfig(cfg, constraints)

	host := req.Host
	var placementResult *placement.PlacementResult

	if host == "" {
		// Liveness-aware placement: probe eligible hosts and pick the best reachable one
		result, err := placement.BestReachableHost(s.db, constraints, 5*time.Second)
		if err != nil {
			if errors.Is(err, placement.ErrNoReachableHost) {
				// No hosts responded — fall back to predictor-aware static placement
				result, err = placement.BestHostWithPredictor(s.db, constraints, nil, predict)
				if err != nil {
					if errors.Is(err, placement.ErrNoEligibleHost) {
						return &SubmitResult{NeedsRental: true}, nil
					}
					return nil, err
				}
			} else if errors.Is(err, placement.ErrNoEligibleHost) {
				return &SubmitResult{NeedsRental: true}, nil
			} else {
				return nil, err
			}
		}
		placementResult = result
		host = result.Host

		oplog.Log(oplog.OpPlacementDecided,
			oplog.WithHost(host),
			oplog.WithDetail(placement.FormatPlacementDetail(result)))
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

	result, err := ops.QueueJob(s.db, params, ops.DefaultOptions())
	if err != nil {
		return nil, err
	}

	// Sync sources after queuing so the CLI returns fast even when the
	// host is unreachable (QueueJob defers in that case). SyncSourcesToHost
	// ignores errors so a slow or offline host won't block the return.
	if !req.NoSync && host != "" && !result.Deferred {
		localDir := workdir.ResolveLocal(req.WorkingDir)
		srcsync.SyncSourcesToHost(host, localDir, req.WorkingDir, req.Inputs)
	}

	return &SubmitResult{
		JobID:           result.JobID,
		Host:            host,
		Deferred:        result.Deferred,
		PlacementResult: placementResult,
	}, nil
}
