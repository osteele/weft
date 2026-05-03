package cmd

import (
	"context"
	"database/sql"
	"os"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/coordinatorrelay"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/secrets"
)

func loadCoordinatorRelay() (*config.Config, *coordinatorrelay.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, err
	}
	client, err := coordinatorrelay.NewClientFromConfig(cfg)
	if err != nil {
		return cfg, nil, err
	}
	return cfg, client, nil
}

func relayEnabled(cfg *config.Config, client *coordinatorrelay.Client) bool {
	return coordinatorrelay.Enabled(cfg) && client != nil
}

func relaySubmitJob(database *sql.DB, cfg *config.Config, client *coordinatorrelay.Client, params ops.QueueJobParams) (int64, *coordinatorrelay.Ack, error) {
	jobID, err := ops.RecordQueuedJob(database, params)
	if err != nil {
		return 0, nil, err
	}
	staged, err := coordinatorrelay.StageSources(context.Background(), cfg, client.R2(), params.WorkingDir, params.Inputs)
	if err != nil {
		return 0, nil, err
	}
	if staged != nil {
		defer func() { _ = removeStageRoot(staged.Root) }()
	}
	launchEnv, err := secrets.ResolveEnvVars(params.EnvVars)
	if err != nil {
		return 0, nil, err
	}

	req := &coordinatorrelay.Request{
		Op:    coordinatorrelay.OpSubmitJob,
		JobID: jobID,
		Submit: &coordinatorrelay.SubmitJobPayload{
			Host:        params.Host,
			WorkingDir:  params.WorkingDir,
			Command:     params.Command,
			Description: params.Description,
			Project:     params.Project,
			EnvVars:     launchEnv,
			Tags:        params.Tags,
			GPU:         params.GPU,
			GPUClass:    params.GPUClass,
			GPUMemGB:    params.GPUMemGB,
			GPUMemMaxGB: params.GPUMemMaxGB,
			DepSpec:     params.DepSpec,
			Inputs:      params.Inputs,
			Outputs:     params.Outputs,
			OutputDirs:  params.OutputDirs,
			Produces:    params.Produces,
			Needs:       params.Needs,
		},
	}
	if staged != nil {
		req.Source = staged.Ref
	}
	ack, err := client.Submit(context.Background(), req)
	return jobID, ack, err
}

func relayUpdateJob(cfg *config.Config, client *coordinatorrelay.Client, job *db.Job, payload *coordinatorrelay.UpdateJobPayload) (*coordinatorrelay.Ack, error) {
	staged, err := coordinatorrelay.StageSources(context.Background(), cfg, client.R2(), job.WorkingDir, job.Inputs)
	if err != nil {
		return nil, err
	}
	if staged != nil {
		defer func() { _ = removeStageRoot(staged.Root) }()
	}
	req := &coordinatorrelay.Request{
		Op:     coordinatorrelay.OpUpdateQueued,
		JobID:  job.ID,
		Update: payload,
	}
	if payload != nil && len(payload.EnvVars) > 0 {
		launchEnv, err := secrets.ResolveEnvVars(payload.EnvVars)
		if err != nil {
			return nil, err
		}
		copyPayload := *payload
		copyPayload.EnvVars = launchEnv
		req.Update = &copyPayload
	}
	if staged != nil {
		req.Source = staged.Ref
	}
	return client.Submit(context.Background(), req)
}

func relayRequeueJob(cfg *config.Config, client *coordinatorrelay.Client, job *db.Job) (*coordinatorrelay.Ack, error) {
	staged, err := coordinatorrelay.StageSources(context.Background(), cfg, client.R2(), job.WorkingDir, job.Inputs)
	if err != nil {
		return nil, err
	}
	if staged != nil {
		defer func() { _ = removeStageRoot(staged.Root) }()
	}
	req := &coordinatorrelay.Request{
		Op:    coordinatorrelay.OpRequeueJob,
		JobID: job.ID,
	}
	if staged != nil {
		req.Source = staged.Ref
	}
	return client.Submit(context.Background(), req)
}

func relaySimpleCommand(client *coordinatorrelay.Client, op string, jobID int64) (*coordinatorrelay.Ack, error) {
	return client.Submit(context.Background(), &coordinatorrelay.Request{
		Op:    op,
		JobID: jobID,
	})
}

func removeStageRoot(root string) error {
	if root == "" {
		return nil
	}
	return os.RemoveAll(root)
}

func stringPtr(s string) *string {
	return &s
}
