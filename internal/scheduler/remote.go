package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/intent"
	"github.com/osteele/weft/internal/oplog"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
)

// RemoteScheduler submits placement intents to the coordinator daemon
// via SSH and polls for outcomes.
type RemoteScheduler struct {
	db        *sql.DB
	appConfig interface{ GetCoordinatorHost() string }
}

// Submit writes a placement intent to the coordinator and records the job locally.
func (s *RemoteScheduler) Submit(_ context.Context, req *SubmitRequest) (*SubmitResult, error) {
	var cfg *config.Config
	if c, ok := s.appConfig.(*config.Config); ok {
		cfg = c
	}
	if cfg == nil {
		return nil, fmt.Errorf("remote scheduler requires app config")
	}

	coordHost := cfg.GetCoordinatorHost()

	// Sync sources to coordinator (hop 1: CLI → coordinator)
	if !req.NoSync && !srcsync.IsLocalHost(coordHost) {
		localDir := workdir.ResolveLocal(req.WorkingDir)
		if localDir != "" {
			if err := srcsync.SyncSourcesToHost(coordHost, localDir, req.WorkingDir, req.Inputs); err != nil {
				log.Printf("sync: source sync to coordinator failed for %s: %v", req.WorkingDir, err)
			}
		}
	}

	// Record job locally with pending_placement status
	jobID, err := db.RecordQueuedWithGPU(s.db, "", req.WorkingDir, req.Command, req.Description, "")
	if err != nil {
		return nil, fmt.Errorf("record job: %w", err)
	}
	if err := db.MarkPendingPlacement(s.db, jobID); err != nil {
		return nil, fmt.Errorf("set pending_placement: %w", err)
	}
	if len(req.Tags) > 0 {
		if err := db.SetJobTags(s.db, jobID, req.Tags); err != nil {
			return nil, fmt.Errorf("set tags: %w", err)
		}
	}
	if len(req.EnvVars) > 0 {
		if err := db.SetJobEnvVars(s.db, jobID, req.EnvVars); err != nil {
			return nil, fmt.Errorf("set env vars: %w", err)
		}
	}
	if len(req.Inputs) > 0 {
		if err := db.SetJobInputs(s.db, jobID, req.Inputs); err != nil {
			return nil, fmt.Errorf("set inputs: %w", err)
		}
	}
	if len(req.Outputs) > 0 {
		if err := db.SetJobOutputs(s.db, jobID, req.Outputs); err != nil {
			return nil, fmt.Errorf("set outputs: %w", err)
		}
	}

	// Build intent
	var gpuMemPtr *int
	if req.GPUMemGB > 0 {
		gpuMemPtr = &req.GPUMemGB
	}
	depSpec := ""
	if req.DepAfter > 0 {
		depSpec = fmt.Sprintf("%d", req.DepAfter)
	}

	hostname, _ := os.Hostname()
	i := &intent.Intent{
		Timestamp: time.Now(),
		Op:        "place",
		IntentID:  uuid.New().String(),
		Source:    hostname,
		Job: intent.IntentJob{
			ID:      jobID,
			Cmd:     req.Command,
			Dir:     req.WorkingDir,
			Desc:    req.Description,
			Env:     req.EnvVars,
			Inputs:  req.Inputs,
			Outputs: req.Outputs,
			Constraints: intent.IntentConstraints{
				GPUClass: req.GPUClass,
				GPUMemGB: req.GPUMemGB,
				Host:     req.Host,
			},
			Tags:     req.Tags,
			DepSpec:  depSpec,
			GPUMemGB: gpuMemPtr,
		},
	}

	// Store intent ID in remote_id for outcome polling
	if err := db.SetJobRemoteID(s.db, jobID, i.IntentID); err != nil {
		return nil, fmt.Errorf("store intent ID: %w", err)
	}

	// Write intent to coordinator
	if err := intent.WriteIntent(coordHost, i); err != nil {
		return &SubmitResult{
			JobID: jobID,
			Host:  "",
		}, fmt.Errorf("submit intent to coordinator: %w", err)
	}

	oplog.Log(oplog.OpCLICommand, oplog.WithDetailf("intent submitted id=%s job=%d", i.IntentID, jobID))

	return &SubmitResult{
		JobID: jobID,
		Host:  "", // will be resolved by coordinator
	}, nil
}
