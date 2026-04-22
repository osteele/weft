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

// resolveCloudNeedsForJob classifies the consumer's --needs specs against the
// current state of each producer and the target rental instance. Specs
// pointing at a producer co-located on targetInstanceID (status running/grace)
// become CloudAfter refs; all others fall through to R2 staging.
//
// The consumer job is re-read from the database so that a stale in-memory
// struct cannot drop Needs captured after submission.
func resolveCloudNeedsForJob(
	ctx context.Context,
	database *sql.DB,
	client *r2.Client,
	job *db.Job,
	targetInstanceID int64,
) ([]cloud.CloudNeed, []cloud.CloudAfterRef, error) {
	if job == nil {
		return nil, nil, fmt.Errorf("job is nil")
	}
	if fresh, err := db.GetJobByID(database, job.ID); err == nil && fresh != nil {
		job.Needs = fresh.Needs
		job.Metadata = fresh.Metadata
	}
	cloudNeeds, cloudAfter, err := ClassifyNeedsForLaunch(ctx, database, client, job, targetInstanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("classify needs for job %d: %w", job.ID, err)
	}
	return cloudNeeds, cloudAfter, nil
}
