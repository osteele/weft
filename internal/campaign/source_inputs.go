package campaign

import (
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/workdir"
)

// SourceInputsByDir groups declared inputs by resolved local working directory.
// Container paths and empty working directories are skipped.
func SourceInputsByDir(jobs []*db.Job) map[string][]string {
	inputsByDir := make(map[string][]string)
	for _, job := range jobs {
		if job == nil {
			continue
		}
		d := workdir.ResolveLocal(job.EffectiveWorkingDir())
		if d == "" || workdir.IsContainerPath(d) {
			continue
		}
		inputsByDir[d] = mergeStringSlices(inputsByDir[d], job.Inputs)
	}
	return inputsByDir
}
