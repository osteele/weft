package services

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/coordinatorrelay"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/r2"
)

// RelayProcessor consumes coordinator relay requests from R2 and applies them
// against the coordinator-local database and LAN-reachable hosts.
type RelayProcessor struct {
	db       *sql.DB
	logger   *slog.Logger
	r2Client *r2.Client
	interval time.Duration
}

func NewRelayProcessor(database *sql.DB, logger *slog.Logger, appCfg *config.Config) *RelayProcessor {
	r2Client := buildRelayR2Client(appCfg)
	return &RelayProcessor{
		db:       database,
		logger:   logger,
		r2Client: r2Client,
		interval: 2 * time.Second,
	}
}

func (p *RelayProcessor) Start(ctx context.Context) {
	if p == nil || p.r2Client == nil {
		return
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.ProcessOnce(ctx)
		}
	}
}

func (p *RelayProcessor) ProcessOnce(ctx context.Context) {
	objects, err := p.r2Client.ListObjects(ctx, controlplane.CoordinatorRelayInboxPrefix())
	if err != nil {
		p.logger.Warn("failed to list relay inbox", "error", err)
		return
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	for _, obj := range objects {
		data, err := p.r2Client.GetObject(ctx, obj.Key)
		if err != nil {
			p.logger.Warn("failed to fetch relay object", "key", obj.Key, "error", err)
			continue
		}
		var req coordinatorrelay.Request
		if err := json.Unmarshal(data, &req); err != nil {
			p.logger.Warn("failed to decode relay object", "key", obj.Key, "error", err)
			continue
		}
		processed, err := db.IsRelayRequestProcessed(p.db, req.RequestID)
		if err != nil {
			p.logger.Warn("failed to check relay processed status", "request_id", req.RequestID, "error", err)
			continue
		}
		if processed {
			if err := p.r2Client.DeleteObject(ctx, obj.Key); err != nil {
				p.logger.Warn("failed to delete stale relay request", "key", obj.Key, "error", err)
			}
			continue
		}
		ack := p.handleRequest(ctx, &req)
		if ack == nil {
			ack = &coordinatorrelay.Ack{
				RequestID:   req.RequestID,
				ProcessedAt: time.Now().UTC().Format(time.RFC3339Nano),
				Accepted:    false,
				JobID:       req.JobID,
				Message:     "unknown relay failure",
			}
		}
		ackData, err := json.Marshal(ack)
		if err != nil {
			p.logger.Warn("failed to marshal relay ack", "request_id", req.RequestID, "error", err)
			continue
		}
		if err := p.r2Client.PutObject(ctx, controlplane.CoordinatorRelayAck(req.RequestID), bytes.NewReader(ackData), "application/json"); err != nil {
			p.logger.Warn("failed to write relay ack", "request_id", req.RequestID, "error", err)
			continue
		}
		if err := db.RecordProcessedRelayRequest(p.db, req.RequestID, req.Op, req.JobID); err != nil {
			p.logger.Warn("failed to persist processed relay request", "request_id", req.RequestID, "error", err)
			continue
		}
		if err := p.r2Client.DeleteObject(ctx, obj.Key); err != nil {
			p.logger.Warn("failed to delete processed relay request", "key", obj.Key, "error", err)
		}
	}
}

func (p *RelayProcessor) handleRequest(ctx context.Context, req *coordinatorrelay.Request) *coordinatorrelay.Ack {
	ack := &coordinatorrelay.Ack{
		RequestID:   req.RequestID,
		ProcessedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Accepted:    false,
		JobID:       req.JobID,
	}
	var err error
	switch req.Op {
	case coordinatorrelay.OpSubmitJob:
		err = p.handleSubmit(ctx, req, ack)
	case coordinatorrelay.OpUpdateQueued:
		err = p.handleUpdate(ctx, req, ack)
	case coordinatorrelay.OpQueuePriority:
		err = p.handlePriority(req, ack)
	case coordinatorrelay.OpKillJob:
		err = p.handleStatus(req, ack, db.StatusKilled)
	case coordinatorrelay.OpCancelJob:
		err = p.handleCancel(req, ack)
	case coordinatorrelay.OpPauseJob:
		err = p.handleStatus(req, ack, db.StatusPaused)
	case coordinatorrelay.OpResumeJob:
		err = p.handleStatus(req, ack, db.StatusRunning)
	case coordinatorrelay.OpRequeueJob:
		err = p.handleRequeue(ctx, req, ack)
	default:
		err = fmt.Errorf("unsupported relay op %q", req.Op)
	}
	if err != nil {
		ack.Message = err.Error()
		return ack
	}
	ack.Accepted = true
	if ack.Message == "" {
		ack.Message = "ok"
	}
	return ack
}

func (p *RelayProcessor) handleSubmit(ctx context.Context, req *coordinatorrelay.Request, ack *coordinatorrelay.Ack) error {
	if req.Submit == nil {
		return fmt.Errorf("submit payload is required")
	}
	params := ops.QueueJobParams{
		Host:        req.Submit.Host,
		WorkingDir:  req.Submit.WorkingDir,
		Command:     req.Submit.Command,
		Description: req.Submit.Description,
		Project:     req.Submit.Project,
		EnvVars:     req.Submit.EnvVars,
		Tags:        req.Submit.Tags,
		GPU:         req.Submit.GPU,
		GPUClass:    req.Submit.GPUClass,
		GPUMemGB:    req.Submit.GPUMemGB,
		GPUMemMaxGB: req.Submit.GPUMemMaxGB,
		DepSpec:     req.Submit.DepSpec,
		Inputs:      req.Submit.Inputs,
		Outputs:     req.Submit.Outputs,
		OutputDirs:  req.Submit.OutputDirs,
		Produces:    req.Submit.Produces,
		Needs:       req.Submit.Needs,
	}
	if err := ops.MirrorQueuedJobWithID(p.db, req.JobID, params); err != nil {
		return err
	}
	job, err := db.GetJobByID(p.db, req.JobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("mirrored job %s not found", ids.FormatJobID(req.JobID))
	}

	host := job.Host
	if host == "" {
		constraints := placement.ConstraintsFromJob(job)
		plan, planErr := placement.Evaluate(placement.EvaluateRequest{
			Constraints: constraints,
			Sources:     []placement.CandidateSource{&placement.OnPremSource{}},
			Database:    p.db,
		})
		if planErr != nil {
			slog.Warn("placement evaluation failed", "component", "relay", "job_id", req.JobID, "error", planErr)
		}
		if planErr == nil && !plan.Unplaced && plan.Cheap != nil && plan.Cheap.OnPrem != nil {
			host = plan.Cheap.OnPrem.Host
			if err := db.UpdateJobHost(p.db, req.JobID, host); err != nil {
				return err
			}
			ack.Host = host
			meta := &db.PlacementMeta{SelectedScore: plan.Cheap.OnPrem.Scores[0].Total}
			_ = db.SetJobPlacementMeta(p.db, req.JobID, meta)
		} else {
			reasons, explainErr := placement.ExplainUnplaced(p.db, constraints)
			if explainErr == nil {
				_ = db.SetJobPlacementReasons(p.db, req.JobID, reasons)
			}
			ack.Message = "job mirrored as unplaced"
			return nil
		}
	}

	if req.Source != nil {
		if err := coordinatorrelay.HydrateSources(ctx, p.r2Client, host, req.Source); err != nil {
			return err
		}
	}
	job, err = db.GetJobByID(p.db, req.JobID)
	if err != nil {
		return err
	}
	result, err := ops.SubmitRecordedQueuedJob(p.db, job, ops.DefaultOptions())
	if err != nil {
		return err
	}
	ack.Host = job.Host
	ack.Message = result.Message
	return nil
}

func (p *RelayProcessor) handleUpdate(ctx context.Context, req *coordinatorrelay.Request, ack *coordinatorrelay.Ack) error {
	if req.Update == nil {
		return fmt.Errorf("update payload is required")
	}
	job, err := db.GetJobByID(p.db, req.JobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found on coordinator", ids.FormatJobID(req.JobID))
	}
	if req.Update.Description != nil {
		if err := db.UpdateJobDescription(p.db, job.ID, *req.Update.Description); err != nil {
			return err
		}
	}
	if req.Update.WorkingDir != nil {
		if err := db.UpdateJobWorkingDir(p.db, job.ID, *req.Update.WorkingDir); err != nil {
			return err
		}
		job.WorkingDir = *req.Update.WorkingDir
	}
	if req.Update.Command != nil {
		if err := db.UpdateJobCommand(p.db, job.ID, *req.Update.Command); err != nil {
			return err
		}
		job.Command = *req.Update.Command
	}
	if req.Update.Project != nil {
		if err := db.SetJobProject(p.db, job.ID, *req.Update.Project); err != nil {
			return err
		}
	}
	if req.Update.ClearEnv || req.Update.EnvVars != nil {
		env := req.Update.EnvVars
		if req.Update.ClearEnv {
			env = nil
		}
		if err := db.SetJobEnvVars(p.db, job.ID, env); err != nil {
			return err
		}
	}
	if req.Update.GPU != nil {
		if err := db.SetJobGPU(p.db, job.ID, *req.Update.GPU); err != nil {
			return err
		}
	}
	if req.Update.GPUClass != nil {
		if err := db.SetJobGPUClass(p.db, job.ID, *req.Update.GPUClass); err != nil {
			return err
		}
	}
	if req.Update.GPUMemGB != nil {
		if err := db.SetJobGPUMemGB(p.db, job.ID, req.Update.GPUMemGB); err != nil {
			return err
		}
	}
	if req.Update.CPUAllotment != nil {
		if err := db.SetJobCPUAllotment(p.db, job.ID, req.Update.CPUAllotment); err != nil {
			return err
		}
	}
	if req.Update.DepSpec != nil {
		if err := db.SetJobDepSpec(p.db, job.ID, *req.Update.DepSpec); err != nil {
			return err
		}
	}
	if req.Update.ClearInputs || req.Update.Inputs != nil {
		inputs := req.Update.Inputs
		if req.Update.ClearInputs {
			inputs = nil
		}
		if err := db.SetJobInputs(p.db, job.ID, inputs); err != nil {
			return err
		}
	}
	if req.Update.Outputs != nil {
		if err := db.SetJobOutputs(p.db, job.ID, req.Update.Outputs); err != nil {
			return err
		}
	}
	if req.Update.OutputDirs != nil {
		if err := db.SetJobOutputDirs(p.db, job.ID, req.Update.OutputDirs); err != nil {
			return err
		}
	}
	if req.Update.Produces != nil {
		if err := db.SetJobProduces(p.db, job.ID, req.Update.Produces); err != nil {
			return err
		}
	}
	if req.Update.Needs != nil {
		if err := db.SetJobNeeds(p.db, job.ID, req.Update.Needs); err != nil {
			return err
		}
	}
	job, err = db.GetJobByID(p.db, job.ID)
	if err != nil {
		return err
	}
	if req.Source != nil {
		if err := coordinatorrelay.HydrateSources(ctx, p.r2Client, job.Host, req.Source); err != nil {
			return err
		}
	}
	result, err := ops.RequestQueueUpdate(p.db, job, ops.DefaultOptions())
	if err != nil {
		return err
	}
	ack.Host = job.Host
	ack.Message = result.Message
	return nil
}

func (p *RelayProcessor) handlePriority(req *coordinatorrelay.Request, ack *coordinatorrelay.Ack) error {
	job, err := db.GetJobByID(p.db, req.JobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found on coordinator", ids.FormatJobID(req.JobID))
	}
	result, err := ops.RequestQueuePriority(p.db, job, ops.DefaultOptions())
	if err != nil {
		return err
	}
	if result.Moved {
		ack.Message = "moved to front"
	} else {
		ack.Message = "already at front"
	}
	ack.Host = job.Host
	return nil
}

func (p *RelayProcessor) handleCancel(req *coordinatorrelay.Request, ack *coordinatorrelay.Ack) error {
	job, err := db.GetJobByID(p.db, req.JobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found on coordinator", ids.FormatJobID(req.JobID))
	}
	result, err := ops.CancelQueuedJob(p.db, job, ops.DefaultOptions())
	if err != nil {
		return err
	}
	ack.Host = job.Host
	ack.Message = result.Message
	return nil
}

func (p *RelayProcessor) handleStatus(req *coordinatorrelay.Request, ack *coordinatorrelay.Ack, targetStatus string) error {
	job, err := db.GetJobByID(p.db, req.JobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found on coordinator", ids.FormatJobID(req.JobID))
	}
	var result ops.Result
	switch targetStatus {
	case db.StatusKilled:
		result, err = ops.KillJob(p.db, job, ops.DefaultOptions())
	default:
		result, err = ops.RequestStatus(p.db, job, targetStatus, ops.TimeoutNormal)
	}
	if err != nil {
		return err
	}
	ack.Host = job.Host
	ack.Message = result.Message
	return nil
}

func (p *RelayProcessor) handleRequeue(ctx context.Context, req *coordinatorrelay.Request, ack *coordinatorrelay.Ack) error {
	job, err := db.GetJobByID(p.db, req.JobID)
	if err != nil {
		return err
	}
	if job == nil {
		return fmt.Errorf("job %s not found on coordinator", ids.FormatJobID(req.JobID))
	}
	if req.Source != nil {
		if err := coordinatorrelay.HydrateSources(ctx, p.r2Client, job.Host, req.Source); err != nil {
			return err
		}
	}
	if err := db.RequeueByID(p.db, job.ID); err != nil {
		return err
	}
	job, err = db.GetJobByID(p.db, job.ID)
	if err != nil {
		return err
	}
	result, err := ops.SubmitRecordedQueuedJob(p.db, job, ops.DefaultOptions())
	if err != nil {
		return err
	}
	ack.Host = job.Host
	ack.Message = result.Message
	return nil
}

func buildRelayR2Client(cfg *config.Config) *r2.Client {
	if cfg == nil || cfg.Vastai.R2.Bucket == "" || cfg.Vastai.R2.AccessKeyID == "" {
		return nil
	}
	client, err := r2.New(r2.Config{
		AccountID:       cfg.Vastai.R2.AccountID,
		AccessKeyID:     cfg.Vastai.R2.AccessKeyID,
		SecretAccessKey: cfg.Vastai.R2.SecretAccessKey,
		Bucket:          cfg.Vastai.R2.Bucket,
	})
	if err != nil {
		return nil
	}
	return client
}

func intValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}
