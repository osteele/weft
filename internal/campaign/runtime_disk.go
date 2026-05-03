package campaign

import "github.com/osteele/weft/internal/db"

// EstimateRuntimeDiskGB returns user-declared transient runtime disk beyond
// declared data inputs and base environment overhead.
func EstimateRuntimeDiskGB(job *db.Job) int {
	if job == nil {
		return 0
	}
	if job.Metadata != nil && job.Metadata.Disk != nil && job.Metadata.Disk.RuntimeDiskGB > 0 {
		return job.Metadata.Disk.RuntimeDiskGB
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
		if job == nil || job.Metadata == nil || job.Metadata.Disk == nil {
			continue
		}
		if job.Metadata.Disk.DiskGB > floor {
			floor = job.Metadata.Disk.DiskGB
		}
	}
	return floor
}
