package orchestration

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/ssh"
)

type recordingKillPutter struct {
	keys       []string
	legacyFail bool
}

func (p *recordingKillPutter) PutObject(_ context.Context, key string, _ io.Reader, _ string) error {
	p.keys = append(p.keys, key)
	if p.legacyFail && strings.HasSuffix(key, "/kill-job") {
		return errors.New("legacy mailbox unavailable")
	}
	return nil
}

func TestWriteCloudJobKillSignalsUsesPerJobKey(t *testing.T) {
	putter := &recordingKillPutter{legacyFail: true}
	if err := writeCloudJobKillSignals(context.Background(), putter, 17, 42); err != nil {
		t.Fatalf("writeCloudJobKillSignals: %v", err)
	}
	want := []string{"instance/42/kill-jobs/17", "instance/42/kill-job"}
	if len(putter.keys) != len(want) {
		t.Fatalf("keys = %v, want %v", putter.keys, want)
	}
	for i := range want {
		if putter.keys[i] != want[i] {
			t.Fatalf("keys = %v, want %v", putter.keys, want)
		}
	}
}

// mockSSHSuccess stubs all SSH commands to succeed, reporting tmux sessions
// as absent so reconciliation resolves the pending status locally.
func mockSSHSuccess(t *testing.T) {
	t.Helper()
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		if strings.Contains(command, "tmux has-session") {
			return "NO\n", "", nil
		}
		return "", "", nil
	})
	t.Cleanup(cleanup)
}

func TestKillOrCancelCloudJob_SetsRequestedStatusOnTerminalInstance(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "test cloud job", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusFailed,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	if err := db.SetRequestedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("SetRequestedStatus(queued): %v", err)
	}

	message, err := KillOrCancelCloudJob(database, jobID, db.StatusKilled)
	if err != nil {
		t.Fatalf("KillOrCancelCloudJob: %v", err)
	}
	if !strings.Contains(message, "killed") {
		t.Fatalf("message = %q, expected kill wording", message)
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if updated.EffectiveStatus() != db.StatusKilled {
		t.Fatalf("effective status = %q, want %q", updated.EffectiveStatus(), db.StatusKilled)
	}
}

func TestKillOrCancelCloudJobCancelsEmptyLaunchingInstance(t *testing.T) {
	database := db.SetupTestDB(t)
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status: db.LaunchStatusLaunching, Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", t.TempDir(), "python train.py", "test", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	previousSignal := signalCloudJobKillForControl
	signaled := false
	signalCloudJobKillForControl = func(_ context.Context, jobID, launchID int64, targetStatus string) (string, error) {
		signaled = true
		return "signaled", nil
	}
	t.Cleanup(func() { signalCloudJobKillForControl = previousSignal })

	message, err := KillOrCancelCloudJob(database, jobID, db.StatusKilled)
	if err != nil {
		t.Fatalf("KillOrCancelCloudJob: %v", err)
	}
	if !strings.Contains(message, "termination is pending provider identity") {
		t.Fatalf("message = %q, want deferred empty-instance termination", message)
	}
	if signaled {
		t.Fatal("empty launch sent a job-level kill signal instead of taking the durable launch cancellation path")
	}
	launch, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if !launch.HasActiveTerminationIntent() {
		t.Fatalf("launch intent = %+v, want active cancellation", launch.TerminationIntent)
	}
}

// Dispatch tests: KillOrCancelJob must forward the cancel/kill intent to
// ops.StopJob rather than hardcoding killed (the full stop-semantics matrix
// lives in internal/ops/kill_test.go).

func TestKillOrCancelJob_CancelRunningJobRecordsCanceled(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	if err != nil {
		t.Fatalf("RecordJobStarting: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("MarkRunningByID: %v", err)
	}
	mockSSHSuccess(t)

	result, err := KillOrCancelJob(database, jobID, db.StatusCanceled, ops.TimeoutFast)
	if err != nil {
		t.Fatalf("KillOrCancelJob: %v", err)
	}
	if !result.Success {
		t.Fatal("result.Success = false")
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if updated.EffectiveStatus() != db.StatusCanceled {
		t.Fatalf("effective status = %q, want %q", updated.EffectiveStatus(), db.StatusCanceled)
	}
	if updated.Status == db.StatusKilled {
		t.Fatalf("cancel was recorded as killed")
	}
}

func TestKillOrCancelJob_KillRunningJobRecordsKilled(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "sleep 100", "test")
	if err != nil {
		t.Fatalf("RecordJobStarting: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("MarkRunningByID: %v", err)
	}
	mockSSHSuccess(t)

	result, err := KillOrCancelJob(database, jobID, db.StatusKilled, ops.TimeoutFast)
	if err != nil {
		t.Fatalf("KillOrCancelJob: %v", err)
	}
	if !result.Success {
		t.Fatal("result.Success = false")
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if updated.EffectiveStatus() != db.StatusKilled {
		t.Fatalf("effective status = %q, want %q", updated.EffectiveStatus(), db.StatusKilled)
	}
}

func TestKillOrCancelJobCancelsDraftJobLocally(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordDraftJob(database, "", "/tmp/project", "python train.py", "draft job", "", "")
	if err != nil {
		t.Fatalf("RecordDraftJob: %v", err)
	}

	result, err := KillOrCancelJob(database, jobID, db.StatusKilled, ops.TimeoutFast)
	if err != nil {
		t.Fatalf("KillOrCancelJob: %v", err)
	}
	if !result.Success {
		t.Fatalf("result.Success = false")
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if updated.EffectiveStatus() != db.StatusCanceled {
		t.Fatalf("effective status = %q, want %q", updated.EffectiveStatus(), db.StatusCanceled)
	}
}
