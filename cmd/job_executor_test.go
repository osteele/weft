package cmd

import (
	"fmt"
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

func TestResolveArtifactNeedsHost(t *testing.T) {
	database := db.SetupTestDB(t)

	hostJobID, err := db.RecordQueued(database, "host-a", "/tmp/project", "echo one", "producer")
	if err != nil {
		t.Fatalf("RecordQueued host-a: %v", err)
	}
	hostJobID2, err := db.RecordQueued(database, "host-a", "/tmp/project", "echo two", "producer")
	if err != nil {
		t.Fatalf("RecordQueued host-a second: %v", err)
	}
	otherHostJobID, err := db.RecordQueued(database, "host-b", "/tmp/project", "echo three", "producer")
	if err != nil {
		t.Fatalf("RecordQueued host-b: %v", err)
	}
	cloudJobID, err := db.RecordQueued(database, "", "/tmp/project", "echo cloud", "producer")
	if err != nil {
		t.Fatalf("RecordQueued cloud: %v", err)
	}

	t.Run("infer host from producers", func(t *testing.T) {
		host, err := resolveArtifactNeedsHost(database, []string{
			fmt.Sprintf("results/model.pt:%d", hostJobID),
			fmt.Sprintf("results/metrics.json:%d", hostJobID2),
		}, "")
		if err != nil {
			t.Fatalf("resolveArtifactNeedsHost: %v", err)
		}
		if host != "host-a" {
			t.Fatalf("host = %q, want host-a", host)
		}
	})

	t.Run("reject mismatched explicit host", func(t *testing.T) {
		_, err := resolveArtifactNeedsHost(database, []string{
			fmt.Sprintf("results/model.pt:%d", hostJobID),
		}, "host-b")
		if err == nil {
			t.Fatal("expected host mismatch error")
		}
	})

	t.Run("reject multiple producer hosts", func(t *testing.T) {
		_, err := resolveArtifactNeedsHost(database, []string{
			fmt.Sprintf("results/model.pt:%d", hostJobID),
			fmt.Sprintf("results/metrics.json:%d", otherHostJobID),
		}, "")
		if err == nil {
			t.Fatal("expected cross-host error")
		}
	})

	t.Run("reject producer without host", func(t *testing.T) {
		_, err := resolveArtifactNeedsHost(database, []string{
			fmt.Sprintf("results/model.pt:%d", cloudJobID),
		}, "")
		if err == nil {
			t.Fatal("expected unresolved producer host error")
		}
	})
}
