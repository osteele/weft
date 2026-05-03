package cmd

import "github.com/osteele/weft/internal/db"

func buildDiskMetadata(diskGB, runtimeDiskGB int) *db.JobDiskMetadata {
	if diskGB <= 0 && runtimeDiskGB <= 0 {
		return nil
	}
	return &db.JobDiskMetadata{
		DiskGB:        diskGB,
		RuntimeDiskGB: runtimeDiskGB,
	}
}
