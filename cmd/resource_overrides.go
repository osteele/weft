package cmd

import (
	"database/sql"

	"github.com/osteele/weft/internal/db"
)

type restartExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

func updateJobCLIResourceOverrides(database restartExecer, job *db.Job, update func(*db.CLIResourceOverrides)) error {
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
	if source.GPUCount != nil {
		v := *source.GPUCount
		clone.GPUCount = &v
	}
	if source.GPUMemGB != nil {
		v := *source.GPUMemGB
		clone.GPUMemGB = &v
	}
	if source.GPUMemStrict != nil {
		v := *source.GPUMemStrict
		clone.GPUMemStrict = &v
	}
	if source.DiskGB != nil {
		v := *source.DiskGB
		clone.DiskGB = &v
	}
	if source.RuntimeDiskGB != nil {
		v := *source.RuntimeDiskGB
		clone.RuntimeDiskGB = &v
	}
	if source.DiskMaxGB != nil {
		v := *source.DiskMaxGB
		clone.DiskMaxGB = &v
	}
	if source.MinSurvival != nil {
		v := *source.MinSurvival
		clone.MinSurvival = &v
	}
	if source.MaxHourlyRateCents != nil {
		v := *source.MaxHourlyRateCents
		clone.MaxHourlyRateCents = &v
	}
	if source.MaxSpendCents != nil {
		v := *source.MaxSpendCents
		clone.MaxSpendCents = &v
	}
	if source.MaxTimeSeconds != nil {
		v := *source.MaxTimeSeconds
		clone.MaxTimeSeconds = &v
	}
	if source.GracePeriodSeconds != nil {
		v := *source.GracePeriodSeconds
		clone.GracePeriodSeconds = &v
	}
	if source.CPUCores != nil {
		v := *source.CPUCores
		clone.CPUCores = &v
	}
	return &clone
}

func setJobCLIGPUOverride(database restartExecer, job *db.Job, gpu string) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		snap.GPU = gpu
		if gpu != "" {
			snap.GPUClass = ""
		}
	})
}

func setJobCLIGPUClassOverride(database restartExecer, job *db.Job, gpuClass string) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		snap.GPUClass = gpuClass
		if gpuClass != "" {
			snap.GPU = ""
		}
	})
}

func setJobCLIGPUMemOverride(database restartExecer, job *db.Job, gpuMemGB *int) error {
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

func setJobCLIGPUMemStrictOverride(database restartExecer, job *db.Job, strict bool) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		v := strict
		snap.GPUMemStrict = &v
	})
}

func setJobCLIDiskOverride(database restartExecer, job *db.Job, diskGB *int) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		snap.DiskGB = cloneIntPtr(diskGB)
	})
}

func setJobCLIDiskMaxOverride(database restartExecer, job *db.Job, diskMaxGB *int) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		snap.DiskMaxGB = cloneIntPtr(diskMaxGB)
	})
}

func setJobCLIRuntimeDiskOverride(database restartExecer, job *db.Job, runtimeDiskGB *int) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		snap.RuntimeDiskGB = cloneIntPtr(runtimeDiskGB)
	})
}

func setJobCLIRunpodCloudTypeOverride(database restartExecer, job *db.Job, cloudType string) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		snap.RunpodCloudType = cloudType
	})
}

func setJobCLIMinSurvivalOverride(database restartExecer, job *db.Job, minSurvival float64) error {
	return updateJobCLIResourceOverrides(database, job, func(snap *db.CLIResourceOverrides) {
		v := minSurvival
		snap.MinSurvival = &v
	})
}

func cloneGPUMemPtr(source *int) *int {
	return cloneIntPtr(source)
}
