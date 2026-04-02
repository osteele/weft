package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestQueueJob_PersistsGPUMemMaxGB(t *testing.T) {
	database := db.SetupTestDB(t)

	gpuMem := 24
	gpuMemMax := 48
	result, err := queueJob(database, queueJobOptions{
		Host:        "test-host",
		WorkingDir:  "/tmp/project",
		Command:     "python train.py",
		GPUClass:    "a100",
		GPUMemGB:    &gpuMem,
		GPUMemMaxGB: &gpuMemMax,
	})
	if err != nil {
		t.Fatalf("queueJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, result.JobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.GPUMemMaxGB == nil || *job.GPUMemMaxGB != gpuMemMax {
		t.Fatalf("GPUMemMaxGB = %v, want %d", job.GPUMemMaxGB, gpuMemMax)
	}
}
