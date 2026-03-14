package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestRestartCommandAliases(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"retry"})
	if err != nil {
		t.Fatalf("find top-level retry: %v", err)
	}
	if cmd != restartCmd {
		t.Fatalf("top-level retry resolved to %q, want restart command", cmd.Name())
	}

	cmd, _, err = jobCmd.Find([]string{"retry"})
	if err != nil {
		t.Fatalf("find job retry: %v", err)
	}
	if cmd != jobRestartCmd {
		t.Fatalf("job retry resolved to %q, want job restart command", cmd.Name())
	}
}

func TestRestartCloudJob_RefreshesProjectMetadata(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	cfg := "inputs = [\"hf:config-model\"]\n\n[outputs]\ndirs = [\"results/\"]\n"
	if err := os.WriteFile(filepath.Join(workDir, ".weft.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write project config: %v", err)
	}

	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "cloud retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	if err := db.SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance id: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, db.StatusFailed, jobID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	if err := restartJob(database, jobID); err != nil {
		t.Fatalf("restartJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if len(job.Inputs) != 1 || job.Inputs[0] != "hf:config-model" {
		t.Fatalf("job inputs = %v, want [hf:config-model]", job.Inputs)
	}
	if len(job.OutputDirs) != 1 || job.OutputDirs[0] != "results/" {
		t.Fatalf("job output dirs = %v, want [results/]", job.OutputDirs)
	}
	if job.CloudInstanceID != nil {
		t.Fatalf("cloud instance id = %v, want nil", job.CloudInstanceID)
	}
}
