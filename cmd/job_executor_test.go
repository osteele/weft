package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
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

func TestQueueJobPEP723HostAxisReachesPinnedHostGate(t *testing.T) {
	inventory.UseTestHosts(t)
	database := db.SetupTestDB(t)
	dir := t.TempDir()
	script := filepath.Join(dir, "train.py")
	if err := os.WriteFile(script, []byte(`# /// script
# [tool.weft]
# cpu-cores = 32
# ///
print("train")
`), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	_, err := queueJob(database, queueJobOptions{
		Host:       "host-beta",
		WorkingDir: dir,
		Command:    "uv run train.py",
	})
	if err == nil {
		t.Fatal("expected PEP 723 CPU floor to reject pinned host")
	}
	if got, want := err.Error(), "cpu gate: host CPU cores 16 below required 32"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestResolveArtifactNeedsPlacement(t *testing.T) {
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

	t.Run("infer host from on-prem producers", func(t *testing.T) {
		host, needs, err := resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("results/model.pt:%d", hostJobID),
			fmt.Sprintf("results/metrics.json:%d", hostJobID2),
		}, "")
		if err != nil {
			t.Fatalf("resolveArtifactNeedsPlacement: %v", err)
		}
		if host != "host-a" {
			t.Fatalf("host = %q, want host-a", host)
		}
		if len(needs) != 2 {
			t.Fatalf("needs = %v, want 2 entries", needs)
		}
	})

	t.Run("reject mismatched explicit host", func(t *testing.T) {
		_, _, err := resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("results/model.pt:%d", hostJobID),
		}, "host-b")
		if err == nil {
			t.Fatal("expected host mismatch error")
		}
	})

	t.Run("reject multiple producer hosts", func(t *testing.T) {
		_, _, err := resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("results/model.pt:%d", hostJobID),
			fmt.Sprintf("results/metrics.json:%d", otherHostJobID),
		}, "")
		if err == nil {
			t.Fatal("expected cross-host error")
		}
	})

	t.Run("unplaced/rental producer passes through without host pinning", func(t *testing.T) {
		host, needs, err := resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("results/model.pt:%d", cloudJobID),
		}, "")
		if err != nil {
			t.Fatalf("resolveArtifactNeedsPlacement: %v", err)
		}
		if host != "" {
			t.Fatalf("host = %q, want empty", host)
		}
		if len(needs) != 1 {
			t.Fatalf("needs = %v, want 1 entry", needs)
		}
	})

	t.Run("reject path that is not in producer's --produces", func(t *testing.T) {
		producerID, err := db.RecordQueued(database, "", "/tmp/project", "echo p", "p")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := db.SetJobProduces(database, producerID, []string{"output/exp_021/model.pt"}); err != nil {
			t.Fatalf("SetJobProduces: %v", err)
		}
		_, _, err = resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("output/exp_022/model.pt:%d", producerID), // typo: 022 not 021
		}, "")
		if err == nil {
			t.Fatal("expected error for path not in producer's --produces")
		}
	})

	t.Run("accept matching path against producer's --produces", func(t *testing.T) {
		producerID, err := db.RecordQueued(database, "", "/tmp/project", "echo p", "p")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := db.SetJobProduces(database, producerID, []string{"output/exp_021/model.pt"}); err != nil {
			t.Fatalf("SetJobProduces: %v", err)
		}
		_, _, err = resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("output/exp_021/model.pt:%d", producerID),
		}, "")
		if err != nil {
			t.Fatalf("resolveArtifactNeedsPlacement: %v", err)
		}
	})

	t.Run("accept any path when producer has no --produces declared", func(t *testing.T) {
		_, _, err := resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("anything/goes.pt:%d", cloudJobID),
		}, "")
		if err != nil {
			t.Fatalf("resolveArtifactNeedsPlacement: %v", err)
		}
	})

	t.Run("accept conventional output path before producer completes", func(t *testing.T) {
		producerID, err := db.RecordQueued(database, "", "/tmp/project", "echo p", "p")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := db.SetJobProduces(database, producerID, []string{"model.pt"}); err != nil {
			t.Fatalf("SetJobProduces: %v", err)
		}
		if err := db.SetJobOutputDirs(database, producerID, []string{"output/"}); err != nil {
			t.Fatalf("SetJobOutputDirs: %v", err)
		}
		// output/metrics.json is not in --produces but falls under a conventional
		// output dir, so it is accepted while the producer has not yet completed.
		_, _, err = resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("output/metrics.json:%d", producerID),
		}, "")
		if err != nil {
			t.Fatalf("resolveArtifactNeedsPlacement: %v", err)
		}
	})

	t.Run("reject path outside declared and conventional outputs", func(t *testing.T) {
		producerID, err := db.RecordQueued(database, "", "/tmp/project", "echo p", "p")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := db.SetJobProduces(database, producerID, []string{"model.pt"}); err != nil {
			t.Fatalf("SetJobProduces: %v", err)
		}
		if err := db.SetJobOutputDirs(database, producerID, []string{"output/"}); err != nil {
			t.Fatalf("SetJobOutputDirs: %v", err)
		}
		_, _, err = resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("results/model.pt:%d", producerID),
		}, "")
		if err == nil {
			t.Fatal("expected error for path outside declared and conventional outputs")
		}
	})

	t.Run("accept recorded artifact path after producer completes", func(t *testing.T) {
		producerID, err := db.RecordQueued(database, "host-a", "/tmp/project", "echo p", "p")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		completeJob(t, database, producerID)
		if err := db.UpsertArtifact(database, db.Artifact{
			JobID:      producerID,
			Name:       "probe.txt",
			Path:       "output/probe.txt",
			StoredPath: fmt.Sprintf("%d/output/probe.txt", producerID),
		}); err != nil {
			t.Fatalf("UpsertArtifact: %v", err)
		}
		_, _, err = resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("output/probe.txt:%d", producerID),
		}, "")
		if err != nil {
			t.Fatalf("resolveArtifactNeedsPlacement: %v", err)
		}
	})

	t.Run("reject path not recorded after producer completes", func(t *testing.T) {
		producerID, err := db.RecordQueued(database, "host-a", "/tmp/project", "echo p", "p")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := db.SetJobOutputDirs(database, producerID, []string{"output/"}); err != nil {
			t.Fatalf("SetJobOutputDirs: %v", err)
		}
		completeJob(t, database, producerID)
		if err := db.UpsertArtifact(database, db.Artifact{
			JobID:      producerID,
			Name:       "probe.txt",
			Path:       "output/probe.txt",
			StoredPath: fmt.Sprintf("%d/output/probe.txt", producerID),
		}); err != nil {
			t.Fatalf("UpsertArtifact: %v", err)
		}
		// output/missing.txt would pass the conventional-dir check, but the
		// producer has completed and did not record it, so it is rejected.
		_, _, err = resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("output/missing.txt:%d", producerID),
		}, "")
		if err == nil {
			t.Fatal("expected error for path not recorded by completed producer")
		}
	})

	t.Run("fall back to conventional check when completed producer has no recorded artifacts", func(t *testing.T) {
		producerID, err := db.RecordQueued(database, "host-a", "/tmp/project", "echo p", "p")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := db.SetJobOutputDirs(database, producerID, []string{"output/"}); err != nil {
			t.Fatalf("SetJobOutputDirs: %v", err)
		}
		completeJob(t, database, producerID)
		// No artifacts recorded (sync lag): fall through to the conventional
		// check, which accepts a path under output/.
		_, _, err = resolveArtifactNeedsPlacement(database, []string{
			fmt.Sprintf("output/result.json:%d", producerID),
		}, "")
		if err != nil {
			t.Fatalf("resolveArtifactNeedsPlacement: %v", err)
		}
	})
}

// completeJob transitions a queued job through running to completed.
func completeJob(t *testing.T, database *sql.DB, jobID int64) {
	t.Helper()
	if err := db.UpdateStatusAndLastSynced(database, jobID, db.StatusRunning); err != nil {
		t.Fatalf("UpdateStatusAndLastSynced running: %v", err)
	}
	if err := db.UpdateStatusAndLastSynced(database, jobID, db.StatusCompleted); err != nil {
		t.Fatalf("UpdateStatusAndLastSynced completed: %v", err)
	}
}

func TestResolveDependencyForTarget(t *testing.T) {
	database := db.SetupTestDB(t)

	localDepID, err := db.RecordQueued(database, "host-a", "/tmp/project", "echo local", "local")
	if err != nil {
		t.Fatalf("RecordQueued local: %v", err)
	}
	cloudDepID, err := db.RecordQueued(database, "", "/tmp/project", "echo cloud", "cloud")
	if err != nil {
		t.Fatalf("RecordQueued cloud: %v", err)
	}

	t.Run("local dependency stays local", func(t *testing.T) {
		localDep, cloudDep, err := resolveDependencyForTarget(database, localDepID, "host-a", false)
		if err != nil {
			t.Fatalf("resolveDependencyForTarget: %v", err)
		}
		if localDep == nil || localDep.JobID != localDepID || localDep.AllowFailure {
			t.Fatalf("unexpected local dep: %+v", localDep)
		}
		if cloudDep != nil {
			t.Fatalf("expected no cloud dep, got %+v", cloudDep)
		}
	})

	t.Run("hostless dependency becomes cloud", func(t *testing.T) {
		localDep, cloudDep, err := resolveDependencyForTarget(database, cloudDepID, "host-a", true)
		if err != nil {
			t.Fatalf("resolveDependencyForTarget: %v", err)
		}
		if localDep != nil {
			t.Fatalf("expected no local dep, got %+v", localDep)
		}
		if cloudDep == nil || cloudDep.JobID != cloudDepID || !cloudDep.AllowFailure {
			t.Fatalf("unexpected cloud dep: %+v", cloudDep)
		}
	})
}
