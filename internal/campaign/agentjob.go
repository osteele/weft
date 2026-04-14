package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"path"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/cloudneeds"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/workdir"
)

var resolveCloudNeedsFunc = cloudneeds.ResolveSpecs

func newAgentJob(job *db.Job, remoteDir string) cloud.AgentJob {
	runID := int64(0)
	if job.LatestRunID != nil {
		runID = *job.LatestRunID
	}
	return cloud.AgentJob{
		ID:         job.ID,
		RunID:      runID,
		Command:    job.EffectiveCommand(),
		Dir:        remoteDir,
		Tags:       append([]string(nil), job.Tags...),
		UsesGPU:    job.UsesGPU(),
		OutputDirs: append([]string(nil), job.OutputDirs...),
		Produces:   append([]string(nil), job.Produces...),
		Needs:      append([]string(nil), job.Needs...),
		Env:        append([]string(nil), job.EnvVars...),
	}
}

func newCloudAgentJob(job *db.Job, remoteDir string) (cloud.AgentJob, error) {
	if job == nil {
		return cloud.AgentJob{}, fmt.Errorf("job is nil")
	}
	if job.LatestRunID == nil || *job.LatestRunID <= 0 {
		return cloud.AgentJob{}, fmt.Errorf("job %d missing non-zero latest_run_id", job.ID)
	}
	agentJob := newAgentJob(job, remoteDir)
	if agentJob.RunID <= 0 {
		return cloud.AgentJob{}, fmt.Errorf("job %d produced invalid run_id=%d", job.ID, agentJob.RunID)
	}
	return agentJob, nil
}

func remoteDirForAgentJob(job *db.Job, localToRemote map[string]string) string {
	localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
	if mapped, ok := localToRemote[localDir]; ok {
		return mapped
	}
	if localDir == "" {
		return ""
	}
	return path.Join(cloud.ProjectRootDir, path.Base(localDir))
}

func resolveCloudNeedsForJob(ctx context.Context, database *sql.DB, client *r2.Client, job *db.Job) ([]cloud.CloudNeed, error) {
	if job == nil {
		return nil, fmt.Errorf("job is nil")
	}
	// Re-read metadata to avoid stale in-memory structs dropping cloud_needs.
	if fresh, err := db.GetJobByID(database, job.ID); err == nil && fresh != nil {
		job.Metadata = fresh.Metadata
	}
	var specs []string
	if job.Metadata != nil && job.Metadata.Dependencies != nil {
		specs = job.Metadata.Dependencies.CloudNeeds
	}
	resolved, err := resolveCloudNeedsFunc(ctx, database, client, specs)
	if err != nil {
		return nil, fmt.Errorf("resolve cloud needs for job %d: %w", job.ID, err)
	}
	return resolved, nil
}
