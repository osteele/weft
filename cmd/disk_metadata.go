package cmd

import (
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func buildDiskMetadata(diskGB, runtimeDiskGB int, project, workingDir, command string) *db.JobDiskMetadata {
	job := &db.Job{
		Project:    project,
		WorkingDir: workingDir,
		Command:    command,
		Metadata: &db.JobMetadata{
			Disk: &db.JobDiskMetadata{RuntimeDiskGB: runtimeDiskGB},
		},
	}
	estimatedRuntimeDiskGB := campaign.EstimateRuntimeDiskGB(job)
	if diskGB <= 0 && runtimeDiskGB <= 0 && estimatedRuntimeDiskGB <= 0 {
		return nil
	}
	return &db.JobDiskMetadata{
		DiskGB:                 diskGB,
		RuntimeDiskGB:          runtimeDiskGB,
		EstimatedRuntimeDiskGB: estimatedRuntimeDiskGB,
	}
}
