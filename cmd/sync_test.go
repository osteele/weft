package cmd

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
)

func TestSyncHostWaitTimeout(t *testing.T) {
	if got := syncHostWaitTimeout(false); got != FastSyncHostTimeout {
		t.Fatalf("syncHostWaitTimeout(false) = %s, want %s", got, FastSyncHostTimeout)
	}
	if got := syncHostWaitTimeout(true); got != NormalSyncHostTimeout {
		t.Fatalf("syncHostWaitTimeout(true) = %s, want %s", got, NormalSyncHostTimeout)
	}
	if NormalSyncHostTimeout <= FastSyncHostTimeout {
		t.Fatalf("NormalSyncHostTimeout = %s, want > %s", NormalSyncHostTimeout, FastSyncHostTimeout)
	}
	if NormalSyncHostTimeout < 5*time.Minute {
		t.Fatalf("NormalSyncHostTimeout = %s, want a generous full-sync timeout", NormalSyncHostTimeout)
	}
}

func TestBase64EncodingPreservesSpecialCharacters(t *testing.T) {
	// Test that base64 encoding properly handles commands with shell-special characters
	// like parentheses which caused issues with nested shell quoting
	testCases := []struct {
		name    string
		command string
	}{
		{
			name:    "parentheses in codec names",
			command: `uv run compression-lab report --codecs "v2f-buckets(64),v2f-buckets(128)"`,
		},
		{
			name:    "single quotes",
			command: `echo 'hello world'`,
		},
		{
			name:    "double quotes",
			command: `echo "hello world"`,
		},
		{
			name:    "dollar signs",
			command: `echo $HOME && ls $TMPDIR`,
		},
		{
			name:    "backticks",
			command: "echo `date`",
		},
		{
			name:    "mixed special chars",
			command: `cd ~/dir && uv run script.py --opt="value(1)" --flag='test'`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			jobLine := "123\t/home/user\t" + tc.command + "\ttest description\t\t"

			// Encode and decode
			encoded := base64.StdEncoding.EncodeToString([]byte(jobLine))
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				t.Fatalf("failed to decode: %v", err)
			}

			if string(decoded) != jobLine {
				t.Errorf("roundtrip failed:\n  got:  %q\n  want: %q", string(decoded), jobLine)
			}

			// Verify encoded string has no shell-special characters
			shellSpecial := []string{"(", ")", "'", "\"", "$", "`", "\\", ";", "&", "|", "<", ">"}
			for _, char := range shellSpecial {
				if strings.Contains(encoded, char) {
					t.Errorf("encoded string contains shell-special character %q: %s", char, encoded)
				}
			}
		})
	}
}

func TestRecordCloudJobCompletion_ClosesAttempt(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	if err := db.SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobCloudInstanceID: %v", err)
	}

	updatedInstanceID, err := recordCloudJobCompletion(database, jobID, 0, 10, 20, "")
	if err != nil {
		t.Fatalf("recordCloudJobCompletion: %v", err)
	}
	if updatedInstanceID != instanceID {
		t.Fatalf("updatedInstanceID = %d, want %d", updatedInstanceID, instanceID)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusCompleted {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusCompleted)
	}
	if job.LatestRunID == nil {
		t.Fatal("job latest_run_id = nil, want non-nil")
	}

	// Verify the attempt (latest_run_id now maps to job_attempts.id)
	if job.Status != db.StatusCompleted {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusCompleted)
	}
	if job.StartTime != 10 {
		t.Fatalf("job start_time = %d, want 10", job.StartTime)
	}
	if job.EndTime == nil || *job.EndTime != 20 {
		t.Fatalf("job end_time = %v, want 20", job.EndTime)
	}

	attempts, err := db.GetJobCloudAttempts(database, jobID)
	if err != nil {
		t.Fatalf("GetJobCloudAttempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempt count = %d, want 1", len(attempts))
	}
	if attempts[0].Outcome != db.AttemptOutcomeCompleted {
		t.Fatalf("attempt outcome = %q, want %q", attempts[0].Outcome, db.AttemptOutcomeCompleted)
	}
	if attempts[0].EndedAt == nil {
		t.Fatal("attempt ended_at = nil, want non-nil")
	}
}

func TestSyncCloudJobResults_RepairsFailedTerminalInstanceJobsWithoutR2(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobCloudInstanceID: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, db.StatusRunning, jobID); err != nil {
		t.Fatalf("set job running: %v", err)
	}

	updated := syncCloudJobResults(&config.Config{}, database, false)
	if updated != 1 {
		t.Fatalf("syncCloudJobResults updated %d rows, want 1", updated)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != db.StatusQueued {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusQueued)
	}

	outcomes, err := db.GetAttemptOutcomesByInstance(database, instanceID)
	if err != nil {
		t.Fatalf("GetAttemptOutcomesByInstance: %v", err)
	}
	if outcomes[jobID] != db.AttemptOutcomeOrphaned {
		t.Fatalf("attempt outcome = %q, want %q", outcomes[jobID], db.AttemptOutcomeOrphaned)
	}
}

func TestJobEligibleForStartedMarker_SkipsQueuedJobWithoutCloudInstance(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE jobs
		 SET status = ?, start_time = ?, cloud_instance_id = NULL, host = ''
		 WHERE id = ?`,
		db.StatusQueued, 123, jobID,
	); err != nil {
		t.Fatalf("seed job state: %v", err)
	}

	runID, ok, err := jobEligibleForStartedMarker(database, jobID)
	if err != nil {
		t.Fatalf("jobEligibleForStartedMarker: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false (runID=%d)", runID)
	}
}

func TestJobEligibleForStartedMarker_SkipsQueuedJobOnFailedInstance(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusFailed,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	if err := db.SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobCloudInstanceID: %v", err)
	}

	runID, ok, err := jobEligibleForStartedMarker(database, jobID)
	if err != nil {
		t.Fatalf("jobEligibleForStartedMarker: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false (runID=%d)", runID)
	}
}

func TestJobEligibleForStartedMarker_AllowsQueuedJobOnRunningInstance(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	if err := db.SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobCloudInstanceID: %v", err)
	}

	runID, ok, err := jobEligibleForStartedMarker(database, jobID)
	if err != nil {
		t.Fatalf("jobEligibleForStartedMarker: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if runID == 0 {
		t.Fatal("runID = 0, want non-zero latest run ID")
	}
}

func TestHostSyncWarningsIncludesQueueDispatchFailure(t *testing.T) {
	warnings := hostSyncWarnings("studio", ops.HostSyncResult{
		QueueDispatchError: "job 289 input staging failed: context deadline exceeded",
	})

	if len(warnings) != 1 {
		t.Fatalf("len(warnings) = %d, want 1", len(warnings))
	}
	if !strings.Contains(warnings[0], "queued jobs were not dispatched on studio") {
		t.Fatalf("warning = %q", warnings[0])
	}
	if !strings.Contains(warnings[0], "job 289 input staging failed") {
		t.Fatalf("warning = %q", warnings[0])
	}
}
