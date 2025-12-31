package ops

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
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

func TestSyncQueueRunnerJobQueuedToRunning(t *testing.T) {
	database := setupTestDB(t)

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
	database := setupTestDB(t)

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
	database := setupTestDB(t)

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

func TestSyncJobSkipsDeadWhenPendingOperations(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "pending-host", "/tmp", "echo pending", "pending job")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	sessionName := "session-pending"
	if _, err := database.Exec(`UPDATE jobs SET session_name = ? WHERE id = ?`, sessionName, jobID); err != nil {
		t.Fatalf("update session: %v", err)
	}

	if err := db.AddDeferredOperation(database, "pending-host", db.OpQueueJob, jobID, "", "{}"); err != nil {
		t.Fatalf("add deferred op: %v", err)
	}

	mockSSHCommands(t, []sshMockResponse{
		{Contains: "tmux has-session", Stdout: "NO\n"},
	})

	job, _ := db.GetJobByID(database, jobID)
	changed, err := SyncJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
	if changed {
		t.Fatalf("expected no change due to pending operations")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusStarting {
		t.Fatalf("expected status to remain starting, got %s", updated.Status)
	}
}

func TestSyncJobQuickMarksDeadWithoutStatus(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "quick-host", "/tmp", "echo quick", "quick job")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	sessionName := "session-quick"
	if _, err := database.Exec(`UPDATE jobs SET session_name = ? WHERE id = ?`, sessionName, jobID); err != nil {
		t.Fatalf("update session: %v", err)
	}

	mockSSHCommands(t, []sshMockResponse{
		{Contains: "tmux has-session", Stdout: "NO\n"},
	})

	job, _ := db.GetJobByID(database, jobID)
	changed, err := SyncJobQuick(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJobQuick: %v", err)
	}
	if !changed {
		t.Fatalf("expected job to be marked dead")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusDead {
		t.Fatalf("expected dead status, got %s", updated.Status)
	}
}

func TestSyncQueueRunnerJobMarksDeadWhenAllProbesFail(t *testing.T) {
	database := setupTestDB(t)

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
		t.Fatalf("expected job to be marked dead")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusDead {
		t.Fatalf("expected dead status, got %s", updated.Status)
	}
}

func TestSyncQueueRunnerJobQuickCompletesJobs(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := db.RecordQueued(database, "quick-queue", "/tmp", "echo", "queue job", "default")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	exitCode := 3
	mock := mockQueueRemote{
		quickStatus: quickStatus{
			ExitCode: &exitCode,
			Mtime:    1700001000,
		},
		metadata: "start_time=1700000500\n",
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	job, _ := db.GetJobByID(database, jobID)
	changed, err := SyncQueueRunnerJobQuick(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJobQuick: %v", err)
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

func TestSyncQueueRunnerJobQuickSkipsDeadWhenPendingOperations(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := db.RecordQueued(database, "skip-host", "/tmp", "echo", "skip job", "default")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := db.AddDeferredOperation(database, "skip-host", db.OpQueueJob, jobID, "", "{}"); err != nil {
		t.Fatalf("add deferred op: %v", err)
	}

	mock := mockQueueRemote{
		quickStatus: quickStatus{State: queueStateDead},
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	job, _ := db.GetJobByID(database, jobID)
	changed, err := SyncQueueRunnerJobQuick(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJobQuick: %v", err)
	}
	if changed {
		t.Fatalf("expected no change when pending operations exist")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusRunning {
		t.Fatalf("expected status to remain running, got %s", updated.Status)
	}
}

type sshMockResponse struct {
	Contains string
	Stdout   string
	Stderr   string
	ExitCode int
}

func mockSSHCommands(t *testing.T, responses []sshMockResponse) {
	handler := func(host, command string) (string, string, int) {
		for _, resp := range responses {
			if resp.Contains == "" || strings.Contains(command, resp.Contains) {
				return resp.Stdout, resp.Stderr, resp.ExitCode
			}
		}
		return "", "", 0
	}

	cleanup := ssh.SetExecCommand(mockSSHExecCommand(handler))
	t.Cleanup(cleanup)
}

func mockSSHExecCommand(handler func(host, command string) (string, string, int)) func(string, ...string) *exec.Cmd {
	return func(name string, args ...string) *exec.Cmd {
		if name != "ssh" {
			return exec.Command(name, args...)
		}
		host, command := parseHostAndCommand(args)
		stdout, stderr, exitCode := handler(host, command)
		return exec.Command("sh", "-c", buildMockCommand(stdout, stderr, exitCode))
	}
}

func parseHostAndCommand(args []string) (string, string) {
	if len(args) >= 2 {
		return args[len(args)-2], args[len(args)-1]
	}
	if len(args) == 1 {
		return "", args[0]
	}
	return "", ""
}

func buildMockCommand(stdout, stderr string, exitCode int) string {
	return fmt.Sprintf("printf '%%s' %s; >&2 printf '%%s' %s; exit %d",
		singleQuote(stdout), singleQuote(stderr), exitCode)
}

func singleQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

// TestSyncJobWithQueueNameAndSessionName tests that a queued job with a SessionName
// (from a previous start_now action that was killed) can still complete correctly
// when the job re-runs via queue runner and creates a status file with a new timestamp.
// The sync should fall back to pattern-based lookup after the exact path fails.
func TestSyncJobWithQueueNameAndSessionName(t *testing.T) {
	database := setupTestDB(t)

	// Create a queued job
	jobID, err := db.RecordQueued(database, "mixed-host", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Simulate start_now: add a session name (as if job was started directly, then killed)
	if _, err := database.Exec(`UPDATE jobs SET session_name = ?, status = ? WHERE id = ?`,
		fmt.Sprintf("rj-%d", jobID), db.StatusRunning, jobID); err != nil {
		t.Fatalf("update session name: %v", err)
	}

	// Mock SSH commands: tmux session doesn't exist, exact status file doesn't exist
	// Note: We return exit code 0 because the SSH functions check stdout content, not exit code
	// For tmux, "NO\n" indicates session doesn't exist
	// For status file read, empty content means file doesn't exist
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "tmux has-session", Stdout: "NO\n"},
		{Contains: "|MTIME|", Stdout: ""}, // Exact status file not found (ReadRemoteFileWithMtime uses |MTIME|)
	})

	// Mock the queue remote pattern-based lookup to show the job completed (exit code 0)
	mock := mockQueueRemote{
		statusExitCode: 0,
		statusMtime:    1700000000,
		statusOption:   Some(true), // Status file exists (job completed via pattern lookup)
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	job, _ := db.GetJobByID(database, jobID)

	// Verify job has both QueueName AND SessionName
	if job.QueueName == "" {
		t.Fatalf("expected job to have QueueName set")
	}
	if job.SessionName == "" {
		t.Fatalf("expected job to have SessionName set (from simulated start_now)")
	}

	// SyncJob should:
	// 1. Check tmux session (doesn't exist)
	// 2. Try exact status file path (doesn't exist due to different timestamp)
	// 3. Fall back to pattern-based lookup (finds the status file)
	// 4. Mark job as completed
	changed, err := SyncJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
	if !changed {
		t.Fatalf("expected job status to change to completed")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusCompleted {
		t.Fatalf("expected completed status, got %s", updated.Status)
	}
	if updated.ExitCode == nil || *updated.ExitCode != 0 {
		t.Fatalf("expected exit code 0, got %v", updated.ExitCode)
	}
}

// TestSyncJobQuickWithQueueNameAndSessionName tests the same scenario for SyncJobQuick
func TestSyncJobQuickWithQueueNameAndSessionName(t *testing.T) {
	database := setupTestDB(t)

	// Create a queued job
	jobID, err := db.RecordQueued(database, "mixed-host-quick", "/tmp", "echo test", "test job", "default")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Simulate start_now: add a session name and set to running
	if _, err := database.Exec(`UPDATE jobs SET session_name = ?, status = ? WHERE id = ?`,
		fmt.Sprintf("rj-%d", jobID), db.StatusRunning, jobID); err != nil {
		t.Fatalf("update session name: %v", err)
	}

	// Mock SSH commands: tmux session doesn't exist, exact status file doesn't exist
	// Note: We return exit code 0 because the SSH functions check stdout content, not exit code
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "tmux has-session", Stdout: "NO\n"},
		{Contains: "|MTIME|", Stdout: ""}, // Exact status file not found (ReadRemoteFileWithMtime uses |MTIME|)
	})

	// Mock the queue remote pattern-based lookup to show the job completed
	mock := mockQueueRemote{
		statusExitCode: 0,
		statusMtime:    1700000000,
		statusOption:   Some(true), // Status file exists (job completed via pattern lookup)
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	job, _ := db.GetJobByID(database, jobID)

	// Verify job has both QueueName AND SessionName
	if job.QueueName == "" || job.SessionName == "" {
		t.Fatalf("expected job to have both QueueName and SessionName set")
	}

	// SyncJobQuick should:
	// 1. Check tmux session (doesn't exist)
	// 2. Try exact status file path (doesn't exist)
	// 3. Fall back to pattern-based lookup (finds the status file)
	// 4. Mark job as completed
	changed, err := SyncJobQuick(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJobQuick: %v", err)
	}
	if !changed {
		t.Fatalf("expected job status to change to completed")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusCompleted {
		t.Fatalf("expected completed status, got %s", updated.Status)
	}
}

func intPtr(i int) *int {
	return &i
}
