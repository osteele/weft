package tui

import (
	"context"
	"database/sql"
	"os"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/coordinatorrelay"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
)

func (m Model) coordinatorRelay() (*config.Config, *coordinatorrelay.Client, error) {
	cfg := m.appConfig
	if cfg == nil {
		var err error
		cfg, err = config.Load()
		if err != nil {
			return nil, nil, err
		}
	}
	client, err := coordinatorrelay.NewClientFromConfig(cfg)
	if err != nil {
		return cfg, nil, err
	}
	if !coordinatorrelay.Enabled(cfg) || client == nil {
		return cfg, nil, nil
	}
	return cfg, client, nil
}

func (m Model) relaySubmitJob(database *sql.DB, params ops.QueueJobParams) (int64, *coordinatorrelay.Ack, error) {
	cfg, client, err := m.coordinatorRelay()
	if err != nil || client == nil {
		return 0, nil, err
	}

	jobID, err := ops.RecordQueuedJob(database, params)
	if err != nil {
		return 0, nil, err
	}
	staged, err := coordinatorrelay.StageSources(context.Background(), cfg, client.R2(), params.WorkingDir, params.Inputs)
	if err != nil {
		return 0, nil, err
	}
	if staged != nil {
		defer func() { _ = removeTUIStageRoot(staged.Root) }()
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
			EnvVars:     params.EnvVars,
			Tags:        params.Tags,
			GPU:         params.GPU,
			GPUClass:    params.GPUClass,
			GPUMemGB:    params.GPUMemGB,
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

func (m Model) relayUpdateJob(job *db.Job, payload *coordinatorrelay.UpdateJobPayload) (*coordinatorrelay.Ack, error) {
	cfg, client, err := m.coordinatorRelay()
	if err != nil || client == nil {
		return nil, err
	}

	staged, err := coordinatorrelay.StageSources(context.Background(), cfg, client.R2(), job.WorkingDir, job.Inputs)
	if err != nil {
		return nil, err
	}
	if staged != nil {
		defer func() { _ = removeTUIStageRoot(staged.Root) }()
	}

	req := &coordinatorrelay.Request{
		Op:     coordinatorrelay.OpUpdateQueued,
		JobID:  job.ID,
		Update: payload,
	}
	if staged != nil {
		req.Source = staged.Ref
	}
	return client.Submit(context.Background(), req)
}

func (m Model) relayRequeueJob(job *db.Job) (*coordinatorrelay.Ack, error) {
	cfg, client, err := m.coordinatorRelay()
	if err != nil || client == nil {
		return nil, err
	}

	staged, err := coordinatorrelay.StageSources(context.Background(), cfg, client.R2(), job.WorkingDir, job.Inputs)
	if err != nil {
		return nil, err
	}
	if staged != nil {
		defer func() { _ = removeTUIStageRoot(staged.Root) }()
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

func (m Model) relaySimpleCommand(op string, jobID int64) (*coordinatorrelay.Ack, error) {
	_, client, err := m.coordinatorRelay()
	if err != nil || client == nil {
		return nil, err
	}
	return client.Submit(context.Background(), &coordinatorrelay.Request{
		Op:    op,
		JobID: jobID,
	})
}

func tuiStringPtr(s string) *string {
	return &s
}

func removeTUIStageRoot(root string) error {
	if root == "" {
		return nil
	}
	return os.RemoveAll(root)
}
