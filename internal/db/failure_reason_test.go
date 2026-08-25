package db

import (
	"testing"
	"time"
)

func TestSanitizeFailureReasonClosedVocabulary(t *testing.T) {
	constants := []string{
		FailureReasonDiskFull,
		FailureReasonOOM,
		FailureReasonGPUOOM,
		FailureReasonSegfault,
		FailureReasonAborted,
		FailureReasonKilledSIGKILL,
		FailureReasonKilledSIGTERM,
		FailureReasonGPUIdle,
		FailureReasonStdoutSilence,
		FailureReasonSetupTimeout,
		FailureReasonRunTimeout,
		FailureReasonCUDADriverTooOld,
		FailureReasonError,
		FailureReasonPrewarmFailed,
		FailureReasonArtifactStageFailed,
		FailureReasonR2ResultsNotSynced,
		FailureReasonSourceRestoreFailed,
		FailureReasonGPUCountPreflightFailed,
		FailureReasonPinnedSourceUnavailable,
		FailureReasonPinnedSourceFetchFailed,
		FailureReasonR2IsolatedSourceUnavailable,
		FailureReasonR2IsolatedSourceFetchFailed,
		FailureReasonSourceProvenanceMismatch,
		FailureReasonCloudAfterFailed,
		FailureReasonInfraPrewarmDownloadFailed,
		FailureReasonInfraCloudArtifactStageFailed,
		FailureReasonInfraCUDAHardwareFault,
		FailureReasonInfraTorchPreflightFailed,
		FailureReasonTorchPreflightEnvironmentFailed,
		FailureReasonTorchPreflightImportFailed,
	}
	for _, reason := range constants {
		t.Run(reason, func(t *testing.T) {
			if !IsKnownFailureReason(reason) {
				t.Fatalf("IsKnownFailureReason(%q) = false", reason)
			}
			if got := SanitizeFailureReason(reason); got != reason {
				t.Fatalf("SanitizeFailureReason(%q) = %q", reason, got)
			}
		})
	}
	for _, reason := range []string{"exit_2", "exit_-1", "signal_killed", "signal_segmentation fault", "signal_9"} {
		t.Run(reason, func(t *testing.T) {
			if !IsKnownFailureReason(reason) || SanitizeFailureReason(reason) != reason {
				t.Fatalf("dynamic reason %q did not round-trip", reason)
			}
		})
	}
	if got := SanitizeFailureReason(""); got != "" {
		t.Fatalf("SanitizeFailureReason(empty) = %q", got)
	}

	invalid := []string{
		"launch completed but job results were not synced from R2",
		"hf prewarm failed exit 1: setup command failed: exit status 1",
		"GPU preflight failed: need 2 ampere+ GPU(s), found 1 available: all ampere+ GPUs are in use",
		"hf prewarm failed exit 1: setup command failed: exit status 1\nprewarm log tail:\n+ annotated-doc==0.0.4\n + anyio==4.13.0\n + fsspec==2026.4.0",
		"error|verified|agent-v1",
		"error\ncontinuation",
		"disk_full: cache volume is full",
		"signal_SIGKILL",
	}
	for _, reason := range invalid {
		t.Run(reason, func(t *testing.T) {
			if IsKnownFailureReason(reason) {
				t.Fatalf("IsKnownFailureReason(%q) = true", reason)
			}
			if got := SanitizeFailureReason(reason); got != FailureReasonError {
				t.Fatalf("SanitizeFailureReason(%q) = %q, want %q", reason, got, FailureReasonError)
			}
		})
	}
}

func TestRepairFailureReasonsIsLosslessAndIdempotent(t *testing.T) {
	database := SetupTestDB(t)
	type seeded struct {
		reason       string
		errorMessage string
		wantReason   string
		wantMessage  string
	}
	cases := []seeded{
		{FailureReasonOOM, "", FailureReasonOOM, ""},
		{"exit_17", "", "exit_17", ""},
		{"launch completed but job results were not synced from R2", "", FailureReasonR2ResultsNotSynced, "launch completed but job results were not synced from R2"},
		{"hf prewarm failed exit 1: exit status 1", "", FailureReasonError, "hf prewarm failed exit 1: exit status 1"},
		{"GPU preflight failed: need 2 a100 GPU(s), found 1", "existing diagnosis", FailureReasonError, "existing diagnosis"},
		{"multi-line failure\nprewarm log tail:\n+ fsspec==2026.5.0", "", FailureReasonError, "multi-line failure\nprewarm log tail:\n+ fsspec==2026.5.0"},
	}

	ids := make([]int64, 0, len(cases))
	for _, tc := range cases {
		jobID, err := RecordQueued(database, "repair-host", "/tmp", "false", "repair test")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`UPDATE job_attempts SET failure_reason = ?, error_message = ? WHERE job_id = ?`, tc.reason, tc.errorMessage, jobID); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, jobID)
	}

	for pass := 1; pass <= 2; pass++ {
		if err := startupRepair(database); err != nil {
			t.Fatalf("repair pass %d: %v", pass, err)
		}
	}
	for i, jobID := range ids {
		var reason, message string
		if err := database.QueryRow(`SELECT COALESCE(failure_reason, ''), COALESCE(error_message, '') FROM job_attempts WHERE job_id = ?`, jobID).Scan(&reason, &message); err != nil {
			t.Fatal(err)
		}
		if reason != cases[i].wantReason || message != cases[i].wantMessage {
			t.Errorf("case %d = reason %q message %q, want %q / %q", i, reason, message, cases[i].wantReason, cases[i].wantMessage)
		}
	}
}

func TestDatabaseFailureReasonWritersSanitizeInputs(t *testing.T) {
	database := SetupTestDB(t)
	remoteJobID, err := RecordQueued(database, "host", "/tmp", "false", "remote state")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetJobRemoteState(database, remoteJobID, "FAILED", "remote prose|shift"); err != nil {
		t.Fatal(err)
	}
	remoteJob, err := GetJobByID(database, remoteJobID)
	if err != nil {
		t.Fatal(err)
	}
	if remoteJob.FailureReason != FailureReasonError {
		t.Fatalf("remote-state failure reason = %q, want %q", remoteJob.FailureReason, FailureReasonError)
	}

	cloudJobID, err := RecordQueuedWithGPU(database, "", "/tmp", "false", "cloud completion", "")
	if err != nil {
		t.Fatal(err)
	}
	launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := SetJobLaunchID(database, cloudJobID, launchID); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordCloudJobCompletion(database, cloudJobID, 1, 1, 2, "completion prose\ncontinuation", "", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	cloudJob, err := GetJobByID(database, cloudJobID)
	if err != nil {
		t.Fatal(err)
	}
	if cloudJob.FailureReason != FailureReasonError {
		t.Fatalf("cloud-completion failure reason = %q, want %q", cloudJob.FailureReason, FailureReasonError)
	}
}
