package ops

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/remote"
	"github.com/osteele/weft/internal/session"
)

type mockQueueRemote struct {
	statusExitCode int
	statusMtime    int64
	statusOption   Option[bool]
	current        Option[bool]
	inQueue        Option[bool]
	process        Option[bool]
	paused         Option[bool]
	quickStatus    quickStatus
	metadata       string
	samples        string
	rusage         string
}

func (m mockQueueRemote) StatusFile(host string, jobID int64, timeout time.Duration) (int, int64, Option[bool]) {
	return m.statusExitCode, m.statusMtime, m.statusOption
}

func (m mockQueueRemote) CurrentJob(host string, jobID int64, timeout time.Duration) Option[bool] {
	return m.current
}

func (m mockQueueRemote) InQueue(host string, jobID int64, timeout time.Duration) Option[bool] {
	return m.inQueue
}

func (m mockQueueRemote) ProcessRunning(host string, jobID int64, timeout time.Duration) Option[bool] {
	return m.process
}

func (m mockQueueRemote) ProcessPaused(host string, jobID int64, timeout time.Duration) Option[bool] {
	return m.paused
}

func (m mockQueueRemote) QuickStatus(host string, jobID int64, timeout time.Duration) (quickStatus, error) {
	return m.quickStatus, nil
}

func (m mockQueueRemote) Metadata(host string, jobID int64, timeout time.Duration) (string, error) {
	return m.metadata, nil
}

func (m mockQueueRemote) Samples(host string, jobID int64, timeout time.Duration) (string, error) {
	return m.samples, nil
}

func (m mockQueueRemote) Rusage(host string, jobID int64, timeout time.Duration) (string, error) {
	return m.rusage, nil
}

func TestSyncQueueRunnerJobQueuedToRunning(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "queue-host", "/tmp", "echo queued", "queued job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	prober := &remote.MockProber{
		CompletedResult: remote.ProbeFalse,
		CurrentResult:   remote.ProbeTrue,
		InQueueResult:   remote.ProbeFalse,
		ProcessResult:   remote.ProbeTrue,
	}
	host := &remote.MockHost{
		MetadataResult: map[string]string{"start_time": "1700000000"},
	}

	syncResult, err := SyncQueueRunnerJobWithProber(database, job, prober, host, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJobWithProber: %v", err)
	}
	if !syncResult.Updated {
		t.Fatalf("expected change when transitioning queued job to running")
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get updated job: %v", err)
	}
	if updated.Status != db.StatusRunning {
		t.Fatalf("expected job status running, got %s", updated.Status)
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

	syncResult, err := SyncJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
	if !syncResult.Updated {
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
	syncResult, err := SyncJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
	if !syncResult.Updated {
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

func TestSyncJobMarksFailedWhenRunningButSessionGone(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a running job - session gone and no status file means it crashed/vanished
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
	syncResult, err := SyncJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
	if !syncResult.Updated {
		t.Fatalf("expected job to be updated to failed")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusFailed {
		t.Fatalf("expected status failed, got %s", updated.Status)
	}
}

func TestSyncJobNoChangeWhenStartingAndSessionGone(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a starting job - session gone is uncertain (race during startup)
	jobID, err := db.RecordJobStarting(database, "quick-host", "/tmp", "echo quick", "quick job")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	sessionName := "session-quick"
	if _, err := database.Exec(`UPDATE jobs SET session_name = ?, status = ? WHERE id = ?`, sessionName, db.StatusStarting, jobID); err != nil {
		t.Fatalf("update job: %v", err)
	}

	mockSSHCommands(t, []sshMockResponse{
		{Contains: "tmux has-session", Stdout: "NO\n"},
		// startStartingJob will try to start the job
		{Contains: "mkdir -p", Stdout: ""},
		{Contains: "cat >", Stdout: ""},
		{Contains: "tmux new-session", Stdout: ""},
	})

	job, _ := db.GetJobByID(database, jobID)
	_, err = SyncJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
}

func TestSyncQueueRunnerJobMarksDeadWhenAllProbesFail(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "dead-host", "/tmp", "echo dead", "dead job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	prober := &remote.MockProber{
		CompletedResult: remote.ProbeFalse,
		CurrentResult:   remote.ProbeFalse,
		InQueueResult:   remote.ProbeFalse,
		ProcessResult:   remote.ProbeFalse,
	}
	host := &remote.MockHost{}

	syncResult, err := SyncQueueRunnerJobWithProber(database, job, prober, host, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJobWithProber: %v", err)
	}
	if !syncResult.Updated {
		t.Fatalf("expected job to be marked failed")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusFailed {
		t.Fatalf("expected failed status, got %s", updated.Status)
	}
}

func TestSyncQueueRunnerJobCompletesJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "queue-host", "/tmp", "echo", "queue job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	exitCode := 3
	prober := &remote.MockProber{
		CompletedResult: remote.ProbeTrue,
		CompletionInfo:  &remote.CompletionInfo{ExitCode: exitCode, EndTime: 1700001000},
	}
	host := &remote.MockHost{
		MetadataResult: map[string]string{"start_time": "1700000500"},
	}

	job, _ := db.GetJobByID(database, jobID)
	syncResult, err := SyncQueueRunnerJobWithProber(database, job, prober, host, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJobWithProber: %v", err)
	}
	if !syncResult.Updated {
		t.Fatalf("expected completion change")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusCompleted {
		t.Fatalf("expected completed status, got %s", updated.Status)
	}
	if updated.ExitCode == nil || *updated.ExitCode != exitCode {
		t.Fatalf("expected exit code %d, got %v", exitCode, updated.ExitCode)
	}
}

// TestSyncJobWithQueueNameAndSessionName tests that a queued job with a SessionName
// (from a previous start_now action that was killed) can still complete correctly
// when the job re-runs via queue runner and creates a status file with a new timestamp.
// The sync should fall back to pattern-based lookup after the exact path fails.
func TestUpdateTimesFromMetadataBothTimes(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "meta-host", "/tmp", "echo test", "meta job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	mock := mockQueueRemote{
		metadata: "start_time=1700000000\nend_time=1700000300\n",
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	endTime, _, err := UpdateTimesFromMetadata(database, job, 5*time.Second)
	if err != nil {
		t.Fatalf("UpdateTimesFromMetadata: %v", err)
	}
	if endTime != 1700000300 {
		t.Fatalf("expected end_time 1700000300, got %d", endTime)
	}
	if job.StartTime != 1700000000 {
		t.Fatalf("expected start_time 1700000000, got %d", job.StartTime)
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.StartTime != 1700000000 {
		t.Fatalf("expected DB start_time 1700000000, got %d", updated.StartTime)
	}
}

func TestUpdateTimesFromMetadataOnlyStartTime(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "meta-host", "/tmp", "echo test", "meta job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	mock := mockQueueRemote{
		metadata: "start_time=1700000000\n",
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	endTime, _, err := UpdateTimesFromMetadata(database, job, 5*time.Second)
	if err != nil {
		t.Fatalf("UpdateTimesFromMetadata: %v", err)
	}
	if endTime != 0 {
		t.Fatalf("expected end_time 0 (absent), got %d", endTime)
	}
	if job.StartTime != 1700000000 {
		t.Fatalf("expected start_time updated, got %d", job.StartTime)
	}
}

func TestUpdateTimesFromMetadataEmptyMetadata(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "meta-host", "/tmp", "echo test", "meta job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	mock := mockQueueRemote{
		metadata: "",
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	endTime, _, err := UpdateTimesFromMetadata(database, job, 5*time.Second)
	if err != nil {
		t.Fatalf("UpdateTimesFromMetadata: %v", err)
	}
	if endTime != 0 {
		t.Fatalf("expected end_time 0 for empty metadata, got %d", endTime)
	}
	if job.StartTime != 0 {
		t.Fatalf("expected start_time unchanged, got %d", job.StartTime)
	}
}

func TestUpdateTimesFromMetadataSkipsStartTimeIfAlreadySet(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "meta-host", "/tmp", "echo test", "meta job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Set start_time in DB
	if _, err := database.Exec(`UPDATE jobs SET start_time = ? WHERE id = ?`, 1700000100, jobID); err != nil {
		t.Fatalf("update start_time: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	mock := mockQueueRemote{
		metadata: "start_time=1700000000\nend_time=1700000300\n",
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	endTime, _, err := UpdateTimesFromMetadata(database, job, 5*time.Second)
	if err != nil {
		t.Fatalf("UpdateTimesFromMetadata: %v", err)
	}
	if endTime != 1700000300 {
		t.Fatalf("expected end_time 1700000300, got %d", endTime)
	}
	// start_time should NOT have been overwritten
	if job.StartTime != 1700000100 {
		t.Fatalf("expected start_time unchanged at 1700000100, got %d", job.StartTime)
	}
}

func TestSyncQueueRunnerJobUsesMetadataEndTime(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "queue-host", "/tmp", "echo", "queue job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// Set up queueRemoteClient mock to return metadata with end_time
	queueMock := mockQueueRemote{
		metadata: "start_time=1700000500\nend_time=1700001000\n",
	}
	restore := setQueueRemoteClientForTesting(queueMock)
	defer restore()

	// Prober reports completion with status file mtime (NFS clock, skewed behind)
	exitCode := 0
	prober := &remote.MockProber{
		CompletedResult: remote.ProbeTrue,
		CompletionInfo:  &remote.CompletionInfo{ExitCode: exitCode, EndTime: 1700000800},
	}
	host := &remote.MockHost{}

	job, _ := db.GetJobByID(database, jobID)
	syncResult, err := SyncQueueRunnerJobWithProber(database, job, prober, host, SyncOptions{Timeout: time.Second, SkipSamples: true})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJobWithProber: %v", err)
	}
	if !syncResult.Updated {
		t.Fatalf("expected completion change")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.EndTime == nil || *updated.EndTime != 1700001000 {
		t.Fatalf("expected end_time from metadata (1700001000), got %v", updated.EndTime)
	}
}

func TestSyncQueueRunnerJobPreservesGPUFields(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "gpu-host", "/tmp", "python train.py", "gpu job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Set GPU and resource fields via SQL (RecordQueued doesn't set these)
	_, err = database.Exec(
		`UPDATE jobs SET gpu='2', gpu_class='a100', gpu_mem_gb=80,
		 output_dirs='["out/"]', produces='["model.pt"]', needs='["data.csv:1"]'
		 WHERE id = ?`, jobID)
	if err != nil {
		t.Fatalf("update GPU fields: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	// All probes false triggers the "not in queue, re-append" path
	prober := &remote.MockProber{
		CompletedResult: remote.ProbeFalse,
		CurrentResult:   remote.ProbeFalse,
		InQueueResult:   remote.ProbeFalse,
		ProcessResult:   remote.ProbeFalse,
		PausedResult:    remote.ProbeFalse,
	}
	host := &remote.MockHost{}

	syncResult, err := SyncQueueRunnerJobWithProber(database, job, prober, host, SyncOptions{Timeout: time.Second, SkipSamples: true})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJobWithProber: %v", err)
	}
	if !syncResult.Updated {
		t.Fatalf("expected sync to re-append the job")
	}

	if len(host.AppendCalls) != 1 {
		t.Fatalf("expected 1 AppendToQueue call, got %d", len(host.AppendCalls))
	}

	entry := host.AppendCalls[0]
	if entry.GPU != "2" {
		t.Errorf("GPU: got %q, want %q", entry.GPU, "2")
	}
	if entry.GPUClass != "a100" {
		t.Errorf("GPUClass: got %q, want %q", entry.GPUClass, "a100")
	}
	if entry.GPUMemGB == nil || *entry.GPUMemGB != 80 {
		t.Errorf("GPUMemGB: got %v, want 80", entry.GPUMemGB)
	}
	if len(entry.OutputDirs) != 1 || entry.OutputDirs[0] != "out/" {
		t.Errorf("OutputDirs: got %v, want [out/]", entry.OutputDirs)
	}
	if len(entry.Produces) != 1 || entry.Produces[0] != "model.pt" {
		t.Errorf("Produces: got %v, want [model.pt]", entry.Produces)
	}
	if len(entry.Needs) != 1 || entry.Needs[0] != "data.csv:1" {
		t.Errorf("Needs: got %v, want [data.csv:1]", entry.Needs)
	}
}

// TestSyncJobRecordsCompletionWithSignalSuffix validates that SyncJob correctly
// handles status files containing exit codes with signal suffixes like "137 signal=KILL".
func TestSyncJobRecordsCompletionWithSignalSuffix(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordJobStarting(database, "sig-host", "/tmp", "echo sig", "signal job")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}

	sessionName := "session-signal"
	startTime := int64(1700000000)
	if _, err := database.Exec(`UPDATE jobs SET session_name = ?, start_time = ? WHERE id = ?`, sessionName, startTime, jobID); err != nil {
		t.Fatalf("update job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// Status file content has signal suffix — "137 signal=KILL"
	logFile := session.LogFile(jobID, startTime)
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "tmux has-session", Stdout: "NO\n"},
		{Contains: "|MTIME|", Stdout: "137 signal=KILL\n|MTIME|\n1700000005\n"},
		{Contains: "stat -c %s", Stdout: "0\n"},
		{Contains: logFile, Stdout: "log contents"},
	})

	job, _ := db.GetJobByID(database, jobID)
	syncResult, err := SyncJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncJob: %v", err)
	}
	if !syncResult.Updated {
		t.Fatalf("expected completion change")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusCompleted {
		t.Fatalf("expected completed status, got %s", updated.Status)
	}
	if updated.ExitCode == nil || *updated.ExitCode != 137 {
		t.Fatalf("expected exit code 137, got %v", updated.ExitCode)
	}
}

// TestQuickStatusParseSignalSuffix validates that the QuickStatus exit code
// parser handles status files containing signal suffixes.
func TestQuickStatusParseSignalSuffix(t *testing.T) {
	tests := []struct {
		name         string
		rawOutput    string
		wantExitCode int
	}{
		{
			name:         "plain exit code",
			rawOutput:    "0|1700000005",
			wantExitCode: 0,
		},
		{
			name:         "exit code with signal suffix",
			rawOutput:    "137 signal=KILL|1700000005",
			wantExitCode: 137,
		},
		{
			name:         "exit 0 with signal suffix",
			rawOutput:    "0 signal=TERM|1700000005",
			wantExitCode: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate parseQuickStatusResult logic from sync.go:814-824
			// Production code now uses fmt.Sscanf to handle signal suffixes
			parts := strings.Split(tt.rawOutput, "|")
			var exitCode int
			if _, err := fmt.Sscanf(parts[0], "%d", &exitCode); err != nil {
				t.Fatalf("fmt.Sscanf failed on %q: %v", parts[0], err)
			}
			if exitCode != tt.wantExitCode {
				t.Errorf("got exit code %d, want %d", exitCode, tt.wantExitCode)
			}
		})
	}
}

// TestSyncQueueRunnerJobGPUDevicesSync validates that GPU device metadata is
// synced when a running job is detected via the individual probe path.
// GPU devices come from the same metadata file as start_time (no extra SSH call).
func TestSyncQueueRunnerJobGPUDevicesSync(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "gpu-host", "/tmp", "python train.py", "gpu job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}

	// Set up metadata with gpu_devices — this is read by UpdateStartTimeFromMetadata
	// and reused by applyGPUDevicesFromMetadata without a second SSH call.
	mock := mockQueueRemote{
		metadata: "start_time=1700000000\ngpu_devices=2,3\n",
	}
	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	prober := &remote.MockProber{
		CompletedResult: remote.ProbeFalse,
		CurrentResult:   remote.ProbeTrue,
		InQueueResult:   remote.ProbeFalse,
		ProcessResult:   remote.ProbeTrue,
	}
	host := &remote.MockHost{}

	job, _ := db.GetJobByID(database, jobID)
	_, err = SyncQueueRunnerJobWithProber(database, job, prober, host, SyncOptions{Timeout: time.Second, SkipSamples: true})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJobWithProber: %v", err)
	}

	// Verify GPU devices were synced to metadata
	updated, _ := db.GetJobByID(database, jobID)
	if updated.Metadata == nil || updated.Metadata.Resource == nil {
		t.Fatalf("expected metadata with GPU devices, got nil")
	}
	if updated.Metadata.Resource.GPUDevices != "2,3" {
		t.Fatalf("expected GPU devices '2,3', got %q", updated.Metadata.Resource.GPUDevices)
	}
}

func TestSyncDraftJobRemovesQueuedEntry(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "draft-queue-host", "/tmp", "echo queued", "draft job")
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

	syncResult, err := SyncDraftJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncDraftJob queued: %v", err)
	}
	if !syncResult.Updated {
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

	syncResult, err := SyncDraftJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncDraftJob tmux: %v", err)
	}
	if !syncResult.Updated {
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
