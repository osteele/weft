package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"path"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/cloudneeds"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/runner"
	"github.com/osteele/weft/internal/secrets"
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
		return cloud.AgentJob{}, fmt.Errorf("job %s missing non-zero latest_run_id", ids.FormatJobID(job.ID))
	}
	agentJob := newAgentJob(job, remoteDir)
	env, err := secrets.ResolveEnvVars(agentJob.Env)
	if err != nil {
		return cloud.AgentJob{}, err
	}
	agentJob.Env = env
	if agentJob.RunID <= 0 {
		return cloud.AgentJob{}, fmt.Errorf("job %s produced invalid run_id=%d", ids.FormatJobID(job.ID), agentJob.RunID)
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
	cloudNeeds, cloudAfter, onPrem, err := ClassifyNeedsForLaunch(ctx, database, client, job, targetInstanceID)
	if err != nil {
		return nil, nil, fmt.Errorf("classify needs for job %s: %w", ids.FormatJobID(job.ID), err)
	}
	if err := assertNeedsClassified(job, cloudNeeds, cloudAfter, onPrem); err != nil {
		return nil, nil, err
	}
	return cloudNeeds, cloudAfter, nil
}

// assertNeedsClassified is a defense-in-depth invariant against silently
// dropped --needs metadata. The wj1213/wj1231/wj1240 incident shipped jobs
// to rental instances with empty cloud_needs manifests because submission
// wrote attempt-scoped metadata to job rows that had no job_attempts row
// yet. The classifier was later fixed to read jobs.needs directly; this
// check ensures any future regression that lets a rental-bound need fall
// out of classification fails at launch instead of crashing inside the
// user's command.
func assertNeedsClassified(
	job *db.Job,
	cloudNeeds []cloud.CloudNeed,
	cloudAfter []cloud.CloudAfterRef,
	onPremSpecs []string,
) error {
	if len(job.Needs) == 0 {
		return nil
	}
	resolvedSpecs := make(map[string]bool, len(cloudNeeds)+len(onPremSpecs))
	for _, n := range cloudNeeds {
		resolvedSpecs[n.Spec] = true
	}
	for _, spec := range onPremSpecs {
		resolvedSpecs[spec] = true
	}
	resolvedAfter := make(map[int64]bool, len(cloudAfter))
	for _, a := range cloudAfter {
		resolvedAfter[a.JobID] = true
	}
	for _, spec := range job.Needs {
		if resolvedSpecs[spec] {
			continue
		}
		parsed, err := runner.ParseNeedsSpec(spec)
		if err != nil {
			return fmt.Errorf("invariant check parse --needs %q: %w", spec, err)
		}
		if resolvedAfter[parsed.Version] {
			continue
		}
		return fmt.Errorf(
			"--needs %q for job %s was not classified (cloudNeeds=%d, cloudAfter=%d, onPrem=%d): "+
				"this indicates dropped dependency metadata; "+
				"refusing to launch because the user command would crash on missing input",
			spec, ids.FormatJobID(job.ID), len(cloudNeeds), len(cloudAfter), len(onPremSpecs),
		)
	}
	return nil
}
