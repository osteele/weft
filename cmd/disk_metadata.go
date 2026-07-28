package cmd

import "github.com/osteele/weft/internal/db"

func buildDiskMetadata(diskGB, diskMaxGB, runtimeDiskGB int) *db.JobDiskMetadata {
	meta := &db.JobDiskMetadata{
		DiskGB:        diskGB,
		DiskMaxGB:     diskMaxGB,
		RuntimeDiskGB: runtimeDiskGB,
	}
	if meta.IsEmpty() {
		return nil
	}
	return meta
}
