package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/spf13/cobra"
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
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance id: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusFailed, jobID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
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
	if job.LaunchID != nil {
		t.Fatalf("cloud instance id = %v, want nil", job.LaunchID)
	}
}

func TestRestartJob_RemovesProcessedTag(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "processed-retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	// Mark as cloud job so restart uses the ResetJobToUnplaced path (no SSH needed)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance id: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusFailed, jobID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}
	if err := db.AddJobTag(database, jobID, db.ProcessedTag); err != nil {
		t.Fatalf("add processed tag: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.HasTag(db.ProcessedTag) {
		t.Fatalf("job still has processed tag after restart")
	}
}

func TestRestartQueuedJob_NoError(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
}

func TestRestartQueuedEndedLaunchJob_CreatesFreshAttemptAndRefreshesMetadata(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# gpu-mem = 24
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}

	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry reset")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	attemptID, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusCanceled)
	if err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET end_time = ?, status = ?, cloud_outcome = ?, pending_status = NULL, pending_at = NULL WHERE id = ?`,
		1_700_000_000, db.StatusCanceled, "canceled", attemptID); err != nil {
		t.Fatalf("mark attempt ended: %v", err)
	}
	oldMem := 8
	if err := db.SetJobGPUMemGB(database, jobID, &oldMem); err != nil {
		t.Fatalf("set initial gpu mem: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued-ended failed: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 26 {
		t.Fatalf("GPUMemGB = %v, want 26 (24 + headroom)", job.GPUMemGB)
	}

	var latestAttemptNumber int
	var latestHost string
	var latestLaunchID any
	var latestStatus string
	var latestEndTime any
	var latestPendingStatus any
	err = database.QueryRow(`
		SELECT attempt_number, host, launch_id, status, end_time, pending_status
		FROM job_attempts
		WHERE job_id = ?
		ORDER BY attempt_number DESC
		LIMIT 1`, jobID).Scan(&latestAttemptNumber, &latestHost, &latestLaunchID, &latestStatus, &latestEndTime, &latestPendingStatus)
	if err != nil {
		t.Fatalf("query latest attempt: %v", err)
	}
	if latestAttemptNumber != 2 {
		t.Fatalf("latest attempt number = %d, want 2", latestAttemptNumber)
	}
	if latestHost != "" {
		t.Fatalf("latest host = %q, want empty for unplaced retry", latestHost)
	}
	if latestLaunchID != nil {
		t.Fatalf("latest launch_id = %v, want nil", latestLaunchID)
	}
	if latestStatus != db.StatusQueued {
		t.Fatalf("latest status = %q, want queued", latestStatus)
	}
	if latestEndTime != nil {
		t.Fatalf("latest end_time = %v, want nil", latestEndTime)
	}
	if latestPendingStatus != db.StatusQueued {
		t.Fatalf("latest pending_status = %v, want queued", latestPendingStatus)
	}

	var priorOutcome string
	if err := database.QueryRow(`SELECT COALESCE(cloud_outcome, '') FROM job_attempts WHERE job_id = ? AND attempt_number = 1`, jobID).
		Scan(&priorOutcome); err != nil {
		t.Fatalf("query prior cloud_outcome: %v", err)
	}
	if priorOutcome != db.AttemptOutcomeSuperseded {
		t.Fatalf("prior cloud_outcome = %q, want %q", priorOutcome, db.AttemptOutcomeSuperseded)
	}
}

func TestRestartQueuedWithCloudHistory_CreatesFreshAttempt(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued retry reset")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	// Historical cloud attempt (not superseded yet): this should trigger fresh retry behavior.
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "H200",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	attempt1, err := db.CreateAttempt(database, jobID, "", &launchID, db.StatusCanceled)
	if err != nil {
		t.Fatalf("create cloud attempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET end_time = ?, status = ?, cloud_outcome = ? WHERE id = ?`,
		1_700_000_000, db.StatusCanceled, db.AttemptOutcomeOrphaned, attempt1); err != nil {
		t.Fatalf("close cloud attempt: %v", err)
	}

	// Current queued open attempt (typical state after a previous retry/no-op path).
	if _, err := db.CreateAttempt(database, jobID, "", nil, db.StatusQueued); err != nil {
		t.Fatalf("create open queued attempt: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued-with-cloud-history failed: %v", err)
	}

	var latestAttemptNumber int
	var latestStatus string
	var latestEndTime any
	err = database.QueryRow(`
		SELECT attempt_number, status, end_time
		FROM job_attempts
		WHERE job_id = ?
		ORDER BY attempt_number DESC
		LIMIT 1`, jobID).Scan(&latestAttemptNumber, &latestStatus, &latestEndTime)
	if err != nil {
		t.Fatalf("query latest attempt: %v", err)
	}
	if latestAttemptNumber != 3 {
		t.Fatalf("latest attempt number = %d, want 3", latestAttemptNumber)
	}
	if latestStatus != db.StatusQueued {
		t.Fatalf("latest status = %q, want queued", latestStatus)
	}
	if latestEndTime != nil {
		t.Fatalf("latest end_time = %v, want nil", latestEndTime)
	}

	var priorOutcome string
	if err := database.QueryRow(`SELECT COALESCE(cloud_outcome, '') FROM job_attempts WHERE job_id = ? AND attempt_number = 1`, jobID).
		Scan(&priorOutcome); err != nil {
		t.Fatalf("query prior cloud_outcome: %v", err)
	}
	if priorOutcome != db.AttemptOutcomeSuperseded {
		t.Fatalf("prior cloud_outcome = %q, want %q", priorOutcome, db.AttemptOutcomeSuperseded)
	}
}

func TestRestartJobRejectsDeterministicPinnedHostGateMismatch(t *testing.T) {
	restore := inventory.SetHosts([]inventory.HostSpec{
		{
			Name: "cool30",
			GPUs: []inventory.GPUSpec{
				{Name: "NVIDIA GeForce RTX 2080 Ti", Class: "rtx2080ti", Memory: "11GB", Indices: []int{0}},
			},
		},
	})
	t.Cleanup(restore)

	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "python train.py", "retry mismatch")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobGPUClass(database, jobID, "ampere+"); err != nil {
		t.Fatalf("set gpu class: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusFailed, jobID); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	err = restartJob(database, jobID, restartOverrides{})
	if err == nil {
		t.Fatal("expected restart to be rejected")
	}
	if got, want := err.Error(), "gpu gate: no GPU matching class ampere+"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestParseRestartOverrides_ParsesGPUAndMem(t *testing.T) {
	restartGPU = "nvidia>=24GB"
	restartGPUClass = ""
	restartGPUMem = 0
	restartGPUMemStrict = false

	cmd := &cobra.Command{Use: "retry"}
	addRestartFlags(cmd)
	if err := cmd.Flags().Set("gpu", "nvidia>=24GB"); err != nil {
		t.Fatalf("set gpu flag: %v", err)
	}

	overrides, err := parseRestartOverrides(cmd)
	if err != nil {
		t.Fatalf("parseRestartOverrides: %v", err)
	}
	if overrides.GPUClass != "nvidia" {
		t.Fatalf("GPUClass = %q, want nvidia", overrides.GPUClass)
	}
	if overrides.GPUMemGB == nil || *overrides.GPUMemGB != 26 {
		t.Fatalf("GPUMemGB = %v, want 26 (24 + headroom)", overrides.GPUMemGB)
	}
}

func TestParseRestartOverrides_StrictKeepsExactMem(t *testing.T) {
	restartGPU = "nvidia>=24GB"
	restartGPUClass = ""
	restartGPUMem = 0
	restartGPUMemStrict = true

	cmd := &cobra.Command{Use: "retry"}
	addRestartFlags(cmd)
	if err := cmd.Flags().Set("gpu", "nvidia>=24GB"); err != nil {
		t.Fatalf("set gpu flag: %v", err)
	}
	if err := cmd.Flags().Set("gpu-mem-strict", "true"); err != nil {
		t.Fatalf("set gpu-mem-strict flag: %v", err)
	}

	overrides, err := parseRestartOverrides(cmd)
	if err != nil {
		t.Fatalf("parseRestartOverrides: %v", err)
	}
	if !overrides.GPUMemStrict {
		t.Fatalf("GPUMemStrict = false, want true")
	}
	if overrides.GPUMemGB == nil || *overrides.GPUMemGB != 24 {
		t.Fatalf("GPUMemGB = %v, want 24", overrides.GPUMemGB)
	}
}

func TestRestartQueuedJob_UpdatesGPUOverrides(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	overrides := restartOverrides{
		GPUClass:  "nvidia",
		GPUMemGB:  intPtrRestart(24),
		HasAny:    true,
		HasGPUMem: true,
	}

	if err := restartJob(database, jobID, overrides); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !strings.EqualFold(job.GPUClass, "nvidia") {
		t.Fatalf("GPUClass = %q, want nvidia", job.GPUClass)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 24 {
		t.Fatalf("GPUMemGB = %v, want 24", job.GPUMemGB)
	}
}

func TestRestartQueuedJob_ReappliesScriptGPUMetadata(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# gpu-mem = 24
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	oldMem := 8
	if err := db.SetJobGPUMemGB(database, jobID, &oldMem); err != nil {
		t.Fatalf("set gpu mem: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 26 {
		t.Fatalf("GPUMemGB = %v, want 26 (24 + headroom)", job.GPUMemGB)
	}
}

func TestRestartQueuedJob_OverrideBeatsScriptMetadata(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# gpu-mem = 24
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	overrides := restartOverrides{
		GPUMemGB:  intPtrRestart(32),
		HasAny:    true,
		HasGPUMem: true,
	}

	if err := restartJob(database, jobID, overrides); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 32 {
		t.Fatalf("GPUMemGB = %v, want 34 (32 + headroom)", job.GPUMemGB)
	}
}

func TestRestartQueuedJob_StrictKeepsExactGPUMem(t *testing.T) {
	database := db.SetupTestDB(t)
	workDir := t.TempDir()
	script := `# /// script
# [tool.weft]
# gpu-mem = 24
# gpu-mem-strict = true
# ///
print("train")
`
	if err := os.WriteFile(filepath.Join(workDir, "train.py"), []byte(script), 0o644); err != nil {
		t.Fatalf("write script: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", workDir, "python train.py", "queued retry")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	if err := restartJob(database, jobID, restartOverrides{}); err != nil {
		t.Fatalf("restartJob queued failed: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != 24 {
		t.Fatalf("GPUMemGB = %v, want 24", job.GPUMemGB)
	}
}

func intPtrRestart(v int) *int {
	return &v
}
