package cmd

import (
	"database/sql"

	"github.com/osteele/weft/internal/db"
)

func updateJobCLIResourceOverrides(database *sql.DB, job *db.Job, update func(*db.CLIResourceOverrides)) error {
	snap := cloneCLIResourceOverrides(job.CLIResourceOverrides)
	update(snap)
	if err := db.SetJobCLIResourceOverrides(database, job.ID, snap); err != nil {
		return err
	}
	if snap.IsEmpty() {
		job.CLIResourceOverrides = nil
	} else {
		job.CLIResourceOverrides = snap
	}
	return nil
}

func cloneCLIResourceOverrides(source *db.CLIResourceOverrides) *db.CLIResourceOverrides {
	if source == nil {
		return &db.CLIResourceOverrides{}
	}
	clone := *source
	if source.GPUMemGB != nil {
		v := *source.GPUMemGB
		clone.GPUMemGB = &v
	}
	if source.GPUMemStrict != nil {
		v := *source.GPUMemStrict
		clone.GPUMemStrict = &v
	}
	return &clone
}

func setJobCLIGPUOverride(database *sql.DB, job *db.Job, gpu string) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		snap.GPU = gpu
		if gpu != "" {
			snap.GPUClass = ""
		}
	})
}

func setJobCLIGPUClassOverride(database *sql.DB, job *db.Job, gpuClass string) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		snap.GPUClass = gpuClass
		if gpuClass != "" {
			snap.GPU = ""
		}
	})
}

func setJobCLIGPUMemOverride(database *sql.DB, job *db.Job, gpuMemGB *int) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		snap.GPUMemGB = cloneGPUMemPtr(gpuMemGB)
		if gpuMemGB == nil {
			snap.GPUMemStrict = nil
			return
		}
		// Edits and retry-time overrides store the effective reservation that
		// is already on the job, so replay it exactly instead of adding
		// headroom again on every later retry.
		strict := true
		snap.GPUMemStrict = &strict
	})
}

func setJobCLIGPUMemStrictOverride(database *sql.DB, job *db.Job, strict bool) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		v := strict
		snap.GPUMemStrict = &v
	})
}

func cloneGPUMemPtr(source *int) *int {
	if source == nil {
		return nil
	}
	v := *source
	return &v
}
