package campaign

import (
	"log/slog"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/workdir"
)

// EstimateRuntimeDiskGB returns user-declared transient runtime disk beyond
// declared data inputs and base environment overhead.
func EstimateRuntimeDiskGB(job *db.Job) int {
	if job == nil {
		return 0
	}
	if job.CLIResourceOverrides != nil && job.CLIResourceOverrides.RuntimeDiskGB != nil {
		return max(0, *job.CLIResourceOverrides.RuntimeDiskGB)
	}
	if job.Metadata != nil && job.Metadata.Disk != nil && job.Metadata.Disk.RuntimeDiskGB > 0 {
		return job.Metadata.Disk.RuntimeDiskGB
	}
	if meta := scanJobScriptMeta(job); meta != nil && meta.RuntimeDiskGB > 0 {
		return meta.RuntimeDiskGB
	}
	return 0
}

func groupRuntimeDiskGB(group InstanceGroup) int {
	runtimeDiskGB := 0
	for _, job := range group.Jobs {
		if job == nil {
			continue
		}
		runtimeDiskGB = max(runtimeDiskGB, EstimateRuntimeDiskGB(job))
	}
	return runtimeDiskGB
}

func groupDiskFloorGB(group InstanceGroup) int {
	floor := 0
	for _, job := range group.Jobs {
		if job == nil {
			continue
		}
		diskGB := 0
		switch {
		case job.CLIResourceOverrides != nil && job.CLIResourceOverrides.DiskGB != nil:
			diskGB = max(0, *job.CLIResourceOverrides.DiskGB)
		case job.Metadata != nil && job.Metadata.Disk != nil && job.Metadata.Disk.DiskGB > 0:
			diskGB = job.Metadata.Disk.DiskGB
		default:
			if meta := scanJobScriptMeta(job); meta != nil && meta.DiskGB > 0 {
				diskGB = meta.DiskGB
			}
		}
		if diskGB > floor {
			floor = diskGB
		}
	}
	return floor
}

func scanJobScriptMeta(job *db.Job) *dataloc.ScriptMeta {
	localDir := workdir.ResolveLocal(job.EffectiveWorkingDir())
	meta, err := dataloc.ScanScriptMeta(localDir, job.Command)
	if err != nil {
		slog.Warn("script metadata error in disk resolution", "component", "campaign", "job_id", job.ID, "error", err)
		return nil
	}
	return meta
}
