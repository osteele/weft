package ops

import (
	"fmt"
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
)

type mockQueueRemote struct {
	statusExitCode int
	statusMtime    int64
	statusOption   Option[bool]
	current        Option[bool]
	inQueue        Option[bool]
	process        Option[bool]
	quickStatus    quickStatus
	metadata       string
	samples        string
}

func (m mockQueueRemote) StatusFile(host string, jobID int64, timeout time.Duration) (int, int64, Option[bool]) {
	return m.statusExitCode, m.statusMtime, m.statusOption
}

func (m mockQueueRemote) CurrentJob(host, queueName string, jobID int64, timeout time.Duration) Option[bool] {
	return m.current
}

func (m mockQueueRemote) InQueue(host, queueName string, jobID int64, timeout time.Duration) Option[bool] {
	return m.inQueue
}

func (m mockQueueRemote) ProcessRunning(host string, jobID int64, timeout time.Duration) Option[bool] {
	return m.process
}

func (m mockQueueRemote) QuickStatus(host, queueName string, jobID int64, timeout time.Duration) (quickStatus, error) {
	return m.quickStatus, nil
}

func (m mockQueueRemote) Metadata(host string, jobID int64, timeout time.Duration) (string, error) {
	return m.metadata, nil
}

func (m mockQueueRemote) Samples(host string, jobID int64, timeout time.Duration) (string, error) {
	return m.samples, nil
}

func TestSyncQueueRunnerJobQueuedToRunning(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "queue-host", "/tmp", "echo queued", "queued job", "default")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	mock := mockQueueRemote{
		statusOption: Some(false),
		current:      Some(true),
		inQueue:      Some(false),
		process:      Some(true),
		metadata:     "start_time=1700000000\n",
	}

	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	changed, err := SyncQueueRunnerJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJob: %v", err)
	}
	if !changed {
		t.Fatalf("expected change when transitioning queued job to running")
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get updated job: %v", err)
	}
	if updated.Status != db.StatusRunning {
		t.Fatalf("expected job status running, got %s", updated.Status)
	}
	if updated.StartTime != 1700000000 {
		t.Fatalf("expected start time updated from metadata, got %d", updated.StartTime)
	}
}

func TestSyncJobMarksStartingAsRunningWhenTmuxAlive(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "tmux-host", "/tmp", "echo run", "tmux job")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	sessionName := "session-running"
	if _, err := database.Exec(`UPDATE jobs SET session_name = ? WHERE id = ?`, sessionName, jobID); err != nil {
		t.Fatalf("update session name: %v", err)
	}

	mockSSHCommands(t, []sshMockResponse{
		{Contains: "tmux has-session", Stdout: "YES\n"},
	})

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	changed, err := SyncJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
	if !changed {
		t.Fatalf("expected job transition to running")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusRunning {
		t.Fatalf("expected running status, got %s", updated.Status)
	}
}

func TestSyncJobRecordsCompletionFromStatusFile(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "status-host", "/tmp", "echo done", "status job")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	sessionName := "session-complete"
	startTime := int64(1700000000)
	if _, err := database.Exec(`UPDATE jobs SET session_name = ?, start_time = ? WHERE id = ?`, sessionName, startTime, jobID); err != nil {
		t.Fatalf("update job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	statusOutput := "0\n|MTIME|\n1700000005\n"
	logFile := session.LogFile(jobID, startTime)

	mockSSHCommands(t, []sshMockResponse{
		{Contains: "tmux has-session", Stdout: "NO\n"},
		{Contains: "|MTIME|", Stdout: statusOutput},
		{Contains: "stat -c %s", Stdout: "0\n"},
		{Contains: logFile, Stdout: "log contents"},
	})

	job, _ := db.GetJobByID(database, jobID)
	changed, err := SyncJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
	if !changed {
		t.Fatalf("expected completion change")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusCompleted {
		t.Fatalf("expected completed status, got %s", updated.Status)
	}
	if updated.ExitCode == nil || *updated.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", updated.ExitCode)
	}
	if updated.EndTime == nil || *updated.EndTime != 1700000005 {
		t.Fatalf("expected end time from mtime, got %v", updated.EndTime)
	}
}

func TestSyncJobNoChangeWhenSessionGoneButNoStatus(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a running job (not starting) - session gone but no status file
	// SyncJob is conservative: doesn't mark dead from missing evidence alone
	jobID, err := db.RecordJobStarting(database, "quick-host", "/tmp", "echo quick", "quick job")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	sessionName := "session-quick"
	if _, err := database.Exec(`UPDATE jobs SET session_name = ?, status = ? WHERE id = ?`, sessionName, db.StatusRunning, jobID); err != nil {
		t.Fatalf("update job: %v", err)
	}

	mockSSHCommands(t, []sshMockResponse{
		{Contains: "tmux has-session", Stdout: "NO\n"},
	})

	job, _ := db.GetJobByID(database, jobID)
	changed, err := SyncJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
	// SyncJob is conservative - doesn't mark dead from just session disappearing
	// This avoids false positives from race conditions
	if changed {
		t.Fatalf("expected no change for uncertain state")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusRunning {
		t.Fatalf("expected status unchanged (running), got %s", updated.Status)
	}
}

func TestSyncQueueRunnerJobMarksDeadWhenAllProbesFail(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "dead-host", "/tmp", "echo dead", "dead job", "default")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	mock := mockQueueRemote{
		statusOption: Some(false),
		current:      Some(false),
		inQueue:      Some(false),
		process:      Some(false),
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	changed, err := SyncQueueRunnerJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJob: %v", err)
	}
	if !changed {
		t.Fatalf("expected job to be marked failed")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusFailed {
		t.Fatalf("expected failed status, got %s", updated.Status)
	}
}

func TestSyncQueueRunnerJobCompletesJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "queue-host", "/tmp", "echo", "queue job", "default")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	exitCode := 3
	mock := mockQueueRemote{
		statusExitCode: exitCode,
		statusMtime:    1700001000,
		statusOption:   Some(true),
		metadata:       "start_time=1700000500\n",
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	job, _ := db.GetJobByID(database, jobID)
	changed, err := SyncQueueRunnerJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJob: %v", err)
	}
	if !changed {
		t.Fatalf("expected completion change")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusCompleted {
		t.Fatalf("expected completed status, got %s", updated.Status)
	}
	if updated.ExitCode == nil || *updated.ExitCode != exitCode {
		t.Fatalf("expected exit code %d, got %v", exitCode, updated.ExitCode)
	}
	if updated.StartTime != 1700000500 {
		t.Fatalf("expected start time from metadata, got %d", updated.StartTime)
	}
}

// TestSyncJobWithQueueNameAndSessionName tests that a queued job with a SessionName
// (from a previous start_now action that was killed) can still complete correctly
// when the job re-runs via queue runner and creates a status file with a new timestamp.
// The sync should fall back to pattern-based lookup after the exact path fails.
func TestSyncDraftJobRemovesQueuedEntry(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "draft-queue-host", "/tmp", "echo queued", "draft job", "default")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	if err := db.MarkJobDraftPending(database, jobID); err != nil {
		t.Fatalf("mark draft pending: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	mock := mockQueueRemote{
		quickStatus: quickStatus{State: queueStateQueued},
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	mockSSHCommands(t, []sshMockResponse{
		{Contains: ".commands", Stdout: ""},
	})

	changed, err := SyncDraftJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncDraftJob queued: %v", err)
	}
	if !changed {
		t.Fatalf("expected SyncDraftJob to update queued draft")
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get updated job: %v", err)
	}
	if updated.Status != db.StatusDraft {
		t.Fatalf("expected draft status, got %s", updated.Status)
	}
	if updated.PendingStatus != nil {
		t.Fatalf("expected pending status cleared, got %v", updated.PendingStatus)
	}
	if updated.LastSyncedStatus != db.StatusDraft {
		t.Fatalf("expected last synced draft, got %s", updated.LastSyncedStatus)
	}
}

func TestSyncDraftJobKillsTmuxSession(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "draft-tmux-host", "/tmp", "echo run", "draft tmux job")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	if _, err := database.Exec(`UPDATE jobs SET session_name = ? WHERE id = ?`, fmt.Sprintf("draft-%d", jobID), jobID); err != nil {
		t.Fatalf("update session name: %v", err)
	}

	if err := db.MarkJobDraftPending(database, jobID); err != nil {
		t.Fatalf("mark draft pending: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	mockSSHCommands(t, []sshMockResponse{
		{Contains: "tmux has-session", Stdout: "YES\n"},
		{Contains: "tmux kill-session", Stdout: ""},
	})

	changed, err := SyncDraftJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncDraftJob tmux: %v", err)
	}
	if !changed {
		t.Fatalf("expected SyncDraftJob to update tmux draft")
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get updated job: %v", err)
	}
	if updated.PendingStatus != nil {
		t.Fatalf("expected pending cleared, got %v", updated.PendingStatus)
	}
	if updated.LastSyncedStatus != db.StatusDraft {
		t.Fatalf("expected last synced draft, got %s", updated.LastSyncedStatus)
	}
}
