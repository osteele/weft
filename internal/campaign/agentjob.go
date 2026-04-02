package campaign

import (
	"path"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/workdir"
)

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
	}
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
