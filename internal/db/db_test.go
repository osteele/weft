package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupRawInitSchemaDB(t *testing.T) *sql.DB {
	t.Helper()

	tmpFile, err := os.CreateTemp("", "weft-db-init-*.db")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	tmpFile.Close()
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	database, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)", tmpFile.Name()))
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestParseCdCommand(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantCmd    string
		wantDir    string
		wantParsed bool
	}{
		{
			name:       "simple cd with &&",
			command:    "cd /foo/bar && python train.py",
			wantCmd:    "python train.py",
			wantDir:    "/foo/bar",
			wantParsed: true,
		},
		{
			name:       "cd with tilde",
			command:    "cd ~/code/project && make build",
			wantCmd:    "make build",
			wantDir:    "~/code/project",
			wantParsed: true,
		},
		{
			name:       "cd with quoted dir",
			command:    "cd '/path/with spaces' && ./run.sh",
			wantCmd:    "./run.sh",
			wantDir:    "/path/with spaces",
			wantParsed: true,
		},
		{
			name:       "cd with double-quoted dir",
			command:    `cd "/path/with spaces" && ./run.sh`,
			wantCmd:    "./run.sh",
			wantDir:    "/path/with spaces",
			wantParsed: true,
		},
		{
			name:       "no cd prefix",
			command:    "python train.py",
			wantCmd:    "",
			wantDir:    "",
			wantParsed: false,
		},
		{
			name:       "cd without &&",
			command:    "cd /foo/bar",
			wantCmd:    "",
			wantDir:    "",
			wantParsed: false,
		},
		{
			name:       "command with && but no cd",
			command:    "make build && make test",
			wantCmd:    "",
			wantDir:    "",
			wantParsed: false,
		},
		{
			name:       "whitespace handling",
			command:    "  cd /foo/bar  &&  python train.py  ",
			wantCmd:    "python train.py",
			wantDir:    "/foo/bar",
			wantParsed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Command: tt.command}
			gotCmd, gotDir := job.ParseCdCommand()

			if tt.wantParsed {
				if gotCmd != tt.wantCmd {
					t.Errorf("ParseCdCommand() cmd = %q, want %q", gotCmd, tt.wantCmd)
				}
				if gotDir != tt.wantDir {
					t.Errorf("ParseCdCommand() dir = %q, want %q", gotDir, tt.wantDir)
				}
			} else {
				if gotCmd != "" || gotDir != "" {
					t.Errorf("ParseCdCommand() = (%q, %q), want empty strings", gotCmd, gotDir)
				}
			}
		})
	}
}

func TestEffectiveCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "cd pattern returns command after &&",
			command: "cd /foo && python train.py",
			want:    "python train.py",
		},
		{
			name:    "no cd pattern returns original",
			command: "python train.py",
			want:    "python train.py",
		},
		{
			name:    "export prefix is stripped",
			command: "export TMPDIR=/tmp && python train.py",
			want:    "python train.py",
		},
		{
			name:    "multiple export prefixes are stripped",
			command: "export A=1 && export B=2 && python train.py",
			want:    "python train.py",
		},
		{
			name:    "cd then export is stripped",
			command: "cd /foo && export TMPDIR=/tmp && python train.py",
			want:    "python train.py",
		},
		{
			name:    "export without && is preserved",
			command: "export FOO=bar",
			want:    "export FOO=bar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Command: tt.command}
			got := job.EffectiveCommand()
			if got != tt.want {
				t.Errorf("EffectiveCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseExportVars(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    []string
	}{
		{
			name:    "no exports",
			command: "python train.py",
			want:    nil,
		},
		{
			name:    "single export",
			command: "export TMPDIR=/tmp && python train.py",
			want:    []string{"TMPDIR=/tmp"},
		},
		{
			name:    "multiple exports",
			command: "export A=1 && export B=2 && python train.py",
			want:    []string{"A=1", "B=2"},
		},
		{
			name:    "cd then export",
			command: "cd /foo && export TMPDIR=/tmp && python train.py",
			want:    []string{"TMPDIR=/tmp"},
		},
		{
			name:    "export without && returns nothing",
			command: "export FOO=bar",
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Command: tt.command}
			got := job.ParseExportVars()
			if len(got) != len(tt.want) {
				t.Errorf("ParseExportVars() = %v, want %v", got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("ParseExportVars()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestEffectiveWorkingDir(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		workingDir string
		want       string
	}{
		{
			name:       "cd pattern returns dir from cd",
			command:    "cd /foo && python train.py",
			workingDir: "~/original",
			want:       "/foo",
		},
		{
			name:       "no cd pattern returns workingDir",
			command:    "python train.py",
			workingDir: "~/original",
			want:       "~/original",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Command: tt.command, WorkingDir: tt.workingDir}
			got := job.EffectiveWorkingDir()
			if got != tt.want {
				t.Errorf("EffectiveWorkingDir() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		seconds  int64
		expected string
	}{
		{0, "0s"},
		{1, "1s"},
		{59, "59s"},
		{60, "1m"},
		{61, "1m 1s"},
		{119, "1m 59s"},
		{120, "2m"},
		{3600, "1h"},
		{3601, "1h 1s"},
		{3661, "1h 1m 1s"},
		{7200, "2h"},
		{7325, "2h 2m 5s"},
		{86400, "24h"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			got := FormatDuration(tt.seconds)
			if got != tt.expected {
				t.Errorf("FormatDuration(%d) = %q, want %q", tt.seconds, got, tt.expected)
			}
		})
	}
}

func TestParseEnvPrefix(t *testing.T) {
	tests := []struct {
		name    string
		command string
		wantCmd string
		wantEnv []string
	}{
		{
			name:    "no env prefix",
			command: "python train.py",
			wantCmd: "python train.py",
			wantEnv: nil,
		},
		{
			name:    "single env var",
			command: "env CUDA_VISIBLE_DEVICES=0 python train.py",
			wantCmd: "python train.py",
			wantEnv: []string{"CUDA_VISIBLE_DEVICES=0"},
		},
		{
			name:    "multiple env vars",
			command: "env CUDA_VISIBLE_DEVICES=0 FOO=bar python train.py",
			wantCmd: "python train.py",
			wantEnv: []string{"CUDA_VISIBLE_DEVICES=0", "FOO=bar"},
		},
		{
			name:    "env without vars",
			command: "env python train.py",
			wantCmd: "env python train.py",
			wantEnv: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotCmd, gotEnv := ParseEnvPrefix(tt.command)
			if gotCmd != tt.wantCmd {
				t.Errorf("ParseEnvPrefix() cmd = %q, want %q", gotCmd, tt.wantCmd)
			}
			if len(gotEnv) != len(tt.wantEnv) {
				t.Errorf("ParseEnvPrefix() env = %v, want %v", gotEnv, tt.wantEnv)
				return
			}
			for i := range gotEnv {
				if gotEnv[i] != tt.wantEnv[i] {
					t.Errorf("ParseEnvPrefix() env[%d] = %q, want %q", i, gotEnv[i], tt.wantEnv[i])
				}
			}
		})
	}
}

func TestGetGPU(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{
			name:    "no GPU",
			command: "python train.py",
			want:    "",
		},
		{
			name:    "env prefix",
			command: "env CUDA_VISIBLE_DEVICES=0 python train.py",
			want:    "0",
		},
		{
			name:    "export prefix",
			command: "export CUDA_VISIBLE_DEVICES=1 && python train.py",
			want:    "1",
		},
		{
			name:    "cd then env",
			command: "cd /foo && env CUDA_VISIBLE_DEVICES=2 python train.py",
			want:    "2",
		},
		{
			name:    "cd then export",
			command: "cd /foo && export CUDA_VISIBLE_DEVICES=3 && python train.py",
			want:    "3",
		},
		{
			name:    "inline assignment",
			command: "CUDA_VISIBLE_DEVICES=4 python train.py",
			want:    "4",
		},
		{
			name:    "multiple GPUs",
			command: "env CUDA_VISIBLE_DEVICES=0,1,2 python train.py",
			want:    "0,1,2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{Command: tt.command}
			got := job.GetGPU()
			if got != tt.want {
				t.Errorf("GetGPU() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseGPUFromCommandString(t *testing.T) {
	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"no GPU", "python train.py", ""},
		{"env prefix", "env CUDA_VISIBLE_DEVICES=0 python train.py", "0"},
		{"inline assignment", "CUDA_VISIBLE_DEVICES=4 python train.py", "4"},
		{"multiple GPUs", "env CUDA_VISIBLE_DEVICES=0,1,2 python train.py", "0,1,2"},
		{"other env vars only", "env FOO=bar python train.py", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseGPUFromCommandString(tt.command)
			if got != tt.want {
				t.Errorf("ParseGPUFromCommandString(%q) = %q, want %q", tt.command, got, tt.want)
			}
		})
	}
}

func TestRecordQueuedAutoPopulatesGPU(t *testing.T) {
	database := SetupTestDB(t)

	// Insert a job with CUDA_VISIBLE_DEVICES in the command but no explicit GPU
	jobID, err := RecordQueuedWithGPU(database, "hostA", "/tmp", "CUDA_VISIBLE_DEVICES=0 python train.py", "test", "")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	if job.GPU != "0" {
		t.Errorf("GPU = %q, want %q", job.GPU, "0")
	}
}

func TestRecordDraftAutoPopulatesGPU(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordDraftJob(database, "hostA", "/tmp", "env CUDA_VISIBLE_DEVICES=1,2 python train.py", "test", "", "")
	if err != nil {
		t.Fatalf("record draft: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	if job.GPU != "1,2" {
		t.Errorf("GPU = %q, want %q", job.GPU, "1,2")
	}
}

func TestRecordQueuedExplicitGPUNotOverridden(t *testing.T) {
	database := SetupTestDB(t)

	// Explicit GPU should not be overridden by command parsing
	jobID, err := RecordQueuedWithGPU(database, "hostA", "/tmp", "CUDA_VISIBLE_DEVICES=0 python train.py", "test", "3")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	if job.GPU != "3" {
		t.Errorf("GPU = %q, want %q (explicit GPU should not be overridden)", job.GPU, "3")
	}
}

func TestRecordUnplacedJob(t *testing.T) {
	database := SetupTestDB(t)

	// An unplaced job is a queued job with host=""
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("record unplaced: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	if job.Status != StatusQueued {
		t.Errorf("Status = %q, want %q", job.Status, StatusQueued)
	}
	if job.Host != "" {
		t.Errorf("Host = %q, want empty", job.Host)
	}
	if job.Command != "python train.py" {
		t.Errorf("Command = %q, want %q", job.Command, "python train.py")
	}
	if job.Description != "GPU training" {
		t.Errorf("Description = %q, want %q", job.Description, "GPU training")
	}
	if job.WorkingDir != "/tmp/project" {
		t.Errorf("WorkingDir = %q, want %q", job.WorkingDir, "/tmp/project")
	}
}

func TestRecordQueuedNormalizesRelativeWorkingDir(t *testing.T) {
	database := SetupTestDB(t)

	root := t.TempDir()
	subdir := filepath.Join(root, "project")
	t.Chdir(root)

	jobID, err := RecordQueuedWithGPU(database, "hostA", "./project", "python train.py", "test", "")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	if job.WorkingDir != subdir {
		t.Fatalf("WorkingDir = %q, want %q", job.WorkingDir, subdir)
	}
}

func TestAssignJobHost(t *testing.T) {
	database := SetupTestDB(t)

	// Create an unplaced job (queued with host="")
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("record unplaced: %v", err)
	}

	// Assign a host
	assigned, err := AssignJobHost(database, jobID, "host-beta")
	if err != nil {
		t.Fatalf("assign: %v", err)
	}
	if !assigned {
		t.Error("expected assignment to succeed")
	}

	// Verify job now has host
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != StatusQueued {
		t.Errorf("Status = %q, want %q", job.Status, StatusQueued)
	}
	if job.Host != "host-beta" {
		t.Errorf("Host = %q, want %q", job.Host, "host-beta")
	}

	// Second assignment should be a no-op (host already set)
	assigned, err = AssignJobHost(database, jobID, "host-alpha")
	if err != nil {
		t.Fatalf("second assign: %v", err)
	}
	if assigned {
		t.Error("second assignment should fail (host already set)")
	}

	// Host should remain host-beta
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job after second assign: %v", err)
	}
	if job.Host != "host-beta" {
		t.Errorf("Host = %q, want %q (should not change)", job.Host, "host-beta")
	}
}

func TestAssignJobHost_FailedJob(t *testing.T) {
	queued := StatusQueued
	tests := []struct {
		name         string
		pending      *string
		wantAssigned bool
	}{
		{"with pending_status=queued", &queued, true},
		{"without pending_status", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database := SetupTestDB(t)

			jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
			if err != nil {
				t.Fatalf("record unplaced: %v", err)
			}
			if _, err := database.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, StatusFailed, jobID); err != nil {
				t.Fatalf("set failed: %v", err)
			}
			if tt.pending != nil {
				if err := SetPendingStatus(database, jobID, *tt.pending); err != nil {
					t.Fatalf("set pending: %v", err)
				}
			}

			assigned, err := AssignJobHost(database, jobID, "host-beta")
			if err != nil {
				t.Fatalf("assign: %v", err)
			}
			if assigned != tt.wantAssigned {
				t.Errorf("assigned = %v, want %v", assigned, tt.wantAssigned)
			}

			if tt.wantAssigned {
				job, err := GetJobByID(database, jobID)
				if err != nil {
					t.Fatalf("get job: %v", err)
				}
				if job.Host != "host-beta" {
					t.Errorf("Host = %q, want %q", job.Host, "host-beta")
				}
			}
		})
	}
}

func TestListUnplacedJobs(t *testing.T) {
	database := SetupTestDB(t)

	// Create an unplaced job (queued, host="")
	unplacedID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "unplaced", "")
	if err != nil {
		t.Fatalf("record unplaced: %v", err)
	}

	// Create a placed job (queued, host="host-beta")
	_, err = RecordQueuedWithGPU(database, "host-beta", "/tmp/project", "echo hello", "placed", "")
	if err != nil {
		t.Fatalf("record placed: %v", err)
	}

	jobs, err := ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("list unplaced: %v", err)
	}

	if len(jobs) != 1 {
		t.Fatalf("expected 1 unplaced job, got %d", len(jobs))
	}
	if jobs[0].ID != unplacedID {
		t.Errorf("expected job ID %d, got %d", unplacedID, jobs[0].ID)
	}
	if jobs[0].Host != "" {
		t.Errorf("expected empty host, got %q", jobs[0].Host)
	}
}

func TestListUnplacedJobsExcludesAssignedCloudJobs(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "cloud assigned", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobCloudInstanceID: %v", err)
	}

	jobs, err := ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("ListUnplacedJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected no unplaced jobs, got %d", len(jobs))
	}
}

func TestListUnplacedJobsExcludesOpenAttemptsOnRunningInstances(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "open attempt", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := InsertJobCloudAttempt(database, jobID, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}

	jobs, err := ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("ListUnplacedJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected no unplaced jobs, got %d", len(jobs))
	}
}

func TestListUnplacedJobsIncludesOpenAttemptsOnTerminalInstances(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusFailed,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "stale open attempt", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := InsertJobCloudAttempt(database, jobID, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}

	jobs, err := ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("ListUnplacedJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 unplaced job, got %d", len(jobs))
	}
	if jobs[0].ID != jobID {
		t.Fatalf("expected job ID %d, got %d", jobID, jobs[0].ID)
	}
}

func TestJobEffectiveStateViewTracksOpenLiveAttempts(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := InsertJobCloudAttempt(database, jobID, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}

	var targetKind, status, attemptStatus string
	var effectiveInstanceID sql.NullInt64
	var attemptInstanceID int64
	var hasOpenLive, isUnplaced int
	err = database.QueryRow(`
		SELECT effective_target_kind, effective_status, effective_cloud_instance_id,
		       current_cloud_attempt_instance_id, current_cloud_attempt_instance_status,
		       has_open_live_cloud_attempt, is_effectively_unplaced
		FROM job_effective_state
		WHERE id = ?`, jobID).
		Scan(&targetKind, &status, &effectiveInstanceID, &attemptInstanceID, &attemptStatus, &hasOpenLive, &isUnplaced)
	if err != nil {
		t.Fatalf("QueryRow(job_effective_state): %v", err)
	}
	if targetKind != string(JobTargetRentalInstance) {
		t.Fatalf("effective_target_kind = %q, want %q", targetKind, JobTargetRentalInstance)
	}
	if status != StatusQueued {
		t.Fatalf("effective_status = %q, want %q", status, StatusQueued)
	}
	if !effectiveInstanceID.Valid || effectiveInstanceID.Int64 != instanceID {
		t.Fatalf("effective_cloud_instance_id = %v, want %d", effectiveInstanceID, instanceID)
	}
	if attemptInstanceID != instanceID || attemptStatus != CloudInstanceStatusRunning {
		t.Fatalf("current attempt = (%d, %q), want (%d, %q)", attemptInstanceID, attemptStatus, instanceID, CloudInstanceStatusRunning)
	}
	if hasOpenLive != 1 || isUnplaced != 0 {
		t.Fatalf("flags = hasOpenLive=%d isUnplaced=%d, want 1/0", hasOpenLive, isUnplaced)
	}
}

func TestJobEffectiveStateViewTreatsTerminalOpenAttemptAsUnplaced(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusFailed,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := InsertJobCloudAttempt(database, jobID, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}

	var targetKind, status, attemptStatus string
	var effectiveInstanceID sql.NullInt64
	var attemptInstanceID int64
	var hasOpenLive, isUnplaced int
	err = database.QueryRow(`
		SELECT effective_target_kind, effective_status, effective_cloud_instance_id,
		       current_cloud_attempt_instance_id, current_cloud_attempt_instance_status,
		       has_open_live_cloud_attempt, is_effectively_unplaced
		FROM job_effective_state
		WHERE id = ?`, jobID).
		Scan(&targetKind, &status, &effectiveInstanceID, &attemptInstanceID, &attemptStatus, &hasOpenLive, &isUnplaced)
	if err != nil {
		t.Fatalf("QueryRow(job_effective_state): %v", err)
	}
	if targetKind != string(JobTargetUnplaced) {
		t.Fatalf("effective_target_kind = %q, want %q", targetKind, JobTargetUnplaced)
	}
	if status != StatusQueued {
		t.Fatalf("effective_status = %q, want %q", status, StatusQueued)
	}
	if effectiveInstanceID.Valid {
		t.Fatalf("effective_cloud_instance_id = %v, want NULL", effectiveInstanceID)
	}
	if attemptInstanceID != instanceID || attemptStatus != CloudInstanceStatusFailed {
		t.Fatalf("current attempt = (%d, %q), want (%d, %q)", attemptInstanceID, attemptStatus, instanceID, CloudInstanceStatusFailed)
	}
	if hasOpenLive != 0 || isUnplaced != 1 {
		t.Fatalf("flags = hasOpenLive=%d isUnplaced=%d, want 0/1", hasOpenLive, isUnplaced)
	}
}

func TestListUnplacedJobsIncludesHostlessRunning(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "hostless running", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatalf("mark queued job running: %v", err)
	}

	jobs, err := ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("list unplaced: %v", err)
	}

	if len(jobs) != 1 {
		t.Fatalf("expected 1 unplaced job, got %d", len(jobs))
	}
	if jobs[0].ID != jobID {
		t.Fatalf("expected job ID %d, got %d", jobID, jobs[0].ID)
	}
	if jobs[0].EffectiveStatus() != StatusQueued {
		t.Fatalf("effective status = %q, want %q", jobs[0].EffectiveStatus(), StatusQueued)
	}
}

func TestSetJobPlacementReasonsRoundTrip(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "unplaced", "")
	if err != nil {
		t.Fatalf("record unplaced: %v", err)
	}
	reasons := []string{"no local host matched gpu-class=L40s, gpu-mem>=20GB", "2 hosts: no L40s GPU"}
	if err := SetJobPlacementReasons(database, jobID, reasons); err != nil {
		t.Fatalf("SetJobPlacementReasons: %v", err)
	}

	jobs, err := ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("ListUnplacedJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 unplaced job, got %d", len(jobs))
	}
	if got := jobs[0].PlacementReasons; strings.Join(got, "\n") != strings.Join(reasons, "\n") {
		t.Fatalf("placement reasons = %v, want %v", got, reasons)
	}
}

func TestMoveQueuedJobToUnplaced(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "host-beta", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	if err := UpdateLastSyncedStatus(database, jobID, StatusQueued); err != nil {
		t.Fatalf("set last synced status: %v", err)
	}
	if err := SetQueuedAtNow(database, jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}

	if err := MoveQueuedJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("MoveQueuedJobToUnplaced: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Host != "" {
		t.Errorf("Host = %q, want empty", job.Host)
	}
	if job.Status != StatusQueued {
		t.Errorf("Status = %q, want %q", job.Status, StatusQueued)
	}
	if job.LastSyncedStatus != "" {
		t.Errorf("LastSyncedStatus = %q, want empty", job.LastSyncedStatus)
	}
	if job.QueueName != "" {
		t.Errorf("QueueName = %q, want empty", job.QueueName)
	}
	if job.QueuedAt != 0 {
		t.Errorf("QueuedAt = %d, want 0", job.QueuedAt)
	}
	if !job.HasTag(TagCloud) {
		t.Errorf("expected cloud tag after move to unplaced, got %v", job.Tags)
	}
	if got := strings.Join(job.PlacementReasons, "\n"); got != "manually moved to unplaced queue" {
		t.Errorf("PlacementReasons = %v, want manual unplaced reason", job.PlacementReasons)
	}
}

func TestMoveQueuedJobToUnplacedKeepsInventoryTag(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "host-beta", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	if err := SetJobTags(database, jobID, []string{TagInventory}); err != nil {
		t.Fatalf("set inventory tag: %v", err)
	}

	if err := MoveQueuedJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("MoveQueuedJobToUnplaced: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !job.HasTag(TagInventory) {
		t.Fatalf("expected inventory tag to remain, got %v", job.Tags)
	}
	if job.HasTag(TagRental) {
		t.Fatalf("inventory job should not gain rental tag, got %v", job.Tags)
	}
}

func TestResetJobToUnplacedSetsReason(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET cloud_instance_id = ?, status = ?, start_time = ? WHERE id = ?`, 42, StatusRunning, time.Now().Unix(), jobID); err != nil {
		t.Fatalf("update job: %v", err)
	}

	if err := ResetJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("ResetJobToUnplaced: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got := strings.Join(job.PlacementReasons, "\n"); got != "cloud instance 42 unavailable; job reset to unplaced queue" {
		t.Fatalf("PlacementReasons = %v", job.PlacementReasons)
	}
}

func TestListActiveOnPremJobs(t *testing.T) {
	database := SetupTestDB(t)

	onPremRunningID, err := RecordJobStarting(database, "cool30", "/tmp/project-alpha", "python train.py", "train")
	if err != nil {
		t.Fatalf("record running on-prem job: %v", err)
	}

	onPremQueuedID, err := RecordQueuedWithGPU(database, "cool30", "/tmp/project-beta", "python eval.py", "eval", "")
	if err != nil {
		t.Fatalf("record queued on-prem job: %v", err)
	}

	cloudJobID, err := RecordQueuedWithGPU(database, "vastai:17", "/tmp/project-cloud", "python cloud.py", "cloud", "")
	if err != nil {
		t.Fatalf("record cloud job: %v", err)
	}
	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	if err := SetJobCloudInstanceID(database, cloudJobID, instanceID); err != nil {
		t.Fatalf("set cloud instance on job: %v", err)
	}

	if _, err := RecordQueuedWithGPU(database, "", "/tmp/project-unplaced", "python wait.py", "unplaced", ""); err != nil {
		t.Fatalf("record unplaced job: %v", err)
	}

	jobs, err := ListActiveOnPremJobs(database)
	if err != nil {
		t.Fatalf("ListActiveOnPremJobs: %v", err)
	}

	if len(jobs) != 2 {
		t.Fatalf("expected 2 on-prem jobs, got %d", len(jobs))
	}
	if jobs[0].ID != onPremRunningID {
		t.Fatalf("first job ID = %d, want %d", jobs[0].ID, onPremRunningID)
	}
	if jobs[1].ID != onPremQueuedID {
		t.Fatalf("second job ID = %d, want %d", jobs[1].ID, onPremQueuedID)
	}
	for _, job := range jobs {
		if job.CloudInstanceID != nil {
			t.Fatalf("unexpected cloud job in on-prem list: %+v", job)
		}
		if job.Host == "" {
			t.Fatalf("unexpected unplaced job in on-prem list: %+v", job)
		}
	}
}

func TestListActiveCloudJobsIncludesOpenLiveAttemptJobs(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "cloud", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := InsertJobCloudAttempt(database, jobID, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}

	jobs, err := ListActiveCloudJobs(database)
	if err != nil {
		t.Fatalf("ListActiveCloudJobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 active cloud job, got %d", len(jobs))
	}
	if jobs[0].ID != jobID {
		t.Fatalf("job ID = %d, want %d", jobs[0].ID, jobID)
	}
	if jobs[0].CloudInstanceID == nil || *jobs[0].CloudInstanceID != instanceID {
		t.Fatalf("cloud_instance_id = %v, want %d", jobs[0].CloudInstanceID, instanceID)
	}
}

func TestListUnprocessedJobsExcludesProcessedDraftAndTombstoned(t *testing.T) {
	database := SetupTestDB(t)

	keepID, err := RecordQueued(database, "cool30", "/tmp/project-alpha", "python train.py", "keep")
	if err != nil {
		t.Fatalf("record keep job: %v", err)
	}

	processedID, err := RecordQueued(database, "cool30", "/tmp/project-beta", "python eval.py", "processed")
	if err != nil {
		t.Fatalf("record processed job: %v", err)
	}
	if err := AddJobTag(database, processedID, ProcessedTag); err != nil {
		t.Fatalf("add processed tag: %v", err)
	}

	draftID, err := RecordQueued(database, "cool30", "/tmp/project-gamma", "python draft.py", "draft")
	if err != nil {
		t.Fatalf("record draft job: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, StatusDraft, draftID); err != nil {
		t.Fatalf("mark draft: %v", err)
	}

	tombstonedID, err := RecordQueued(database, "cool30", "/tmp/project-delta", "python old.py", "old")
	if err != nil {
		t.Fatalf("record tombstoned job: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET tombstoned = 1 WHERE id = ?`, tombstonedID); err != nil {
		t.Fatalf("mark tombstoned: %v", err)
	}

	jobs, err := ListUnprocessedJobs(database)
	if err != nil {
		t.Fatalf("ListUnprocessedJobs: %v", err)
	}

	if len(jobs) != 1 {
		t.Fatalf("expected 1 unprocessed job, got %d", len(jobs))
	}
	if jobs[0].ID != keepID {
		t.Fatalf("unprocessed job ID = %d, want %d", jobs[0].ID, keepID)
	}
}

func TestListRecentTerminalJobsIncludesRequestedStatusesAndCutoff(t *testing.T) {
	database := SetupTestDB(t)
	now := time.Now().Unix()
	cutoff := now - 3600

	completedOKID, err := RecordJobStarting(database, "cool30", "/tmp/project-alpha", "python ok.py", "ok")
	if err != nil {
		t.Fatalf("record completed ok: %v", err)
	}
	if err := RecordCompletionByID(database, completedOKID, 0, now-10); err != nil {
		t.Fatalf("complete ok: %v", err)
	}

	completedFailID, err := RecordJobStarting(database, "cool30", "/tmp/project-beta", "python fail.py", "fail")
	if err != nil {
		t.Fatalf("record completed fail: %v", err)
	}
	if err := RecordCompletionByID(database, completedFailID, 3, now-20); err != nil {
		t.Fatalf("complete fail: %v", err)
	}

	deadID, err := RecordJobStarting(database, "cool30", "/tmp/project-gamma", "python dead.py", "dead")
	if err != nil {
		t.Fatalf("record dead: %v", err)
	}
	if err := MarkDeadByID(database, deadID); err != nil {
		t.Fatalf("mark dead: %v", err)
	}
	// Set a specific end_time on the attempt (not the jobs table)
	if _, err := database.Exec(`UPDATE job_attempts SET end_time = ? WHERE job_id = ?`, now-30, deadID); err != nil {
		t.Fatalf("set dead end_time: %v", err)
	}

	killedID, err := RecordQueued(database, "cool30", "/tmp/project-delta", "python killed.py", "killed")
	if err != nil {
		t.Fatalf("record killed: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET status = ?, end_time = ? WHERE id = ?`, StatusKilled, now-40, killedID); err != nil {
		t.Fatalf("mark killed: %v", err)
	}

	canceledID, err := RecordQueued(database, "cool30", "/tmp/project-epsilon", "python canceled.py", "canceled")
	if err != nil {
		t.Fatalf("record canceled: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET status = ?, end_time = ? WHERE id = ?`, StatusCanceled, now-50, canceledID); err != nil {
		t.Fatalf("mark canceled: %v", err)
	}

	oldID, err := RecordJobStarting(database, "cool30", "/tmp/project-old", "python old.py", "old")
	if err != nil {
		t.Fatalf("record old: %v", err)
	}
	if err := RecordCompletionByID(database, oldID, 0, cutoff-1); err != nil {
		t.Fatalf("complete old: %v", err)
	}

	jobs, err := ListRecentTerminalJobs(database, cutoff)
	if err != nil {
		t.Fatalf("ListRecentTerminalJobs: %v", err)
	}

	gotIDs := make([]int64, 0, len(jobs))
	for _, job := range jobs {
		gotIDs = append(gotIDs, job.ID)
		if job.EndTime == nil || *job.EndTime < cutoff {
			t.Fatalf("job %d has end_time before cutoff: %+v", job.ID, job.EndTime)
		}
	}
	wantIDs := []int64{completedOKID, completedFailID, deadID, killedID, canceledID}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("recent terminal count = %d, want %d (ids=%v)", len(gotIDs), len(wantIDs), gotIDs)
	}
	for _, wantID := range wantIDs {
		found := false
		for _, gotID := range gotIDs {
			if gotID == wantID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing job %d from recent terminal jobs: %v", wantID, gotIDs)
		}
	}
}

func TestFilterByFreshStatusPassesNonInventoryJobs(t *testing.T) {
	cloudInstanceID := int64(17)
	jobs := []*Job{
		{ID: 1, Host: "studio", Status: StatusQueued}, // inventory — matches
		{ID: 2, Host: "", Status: StatusQueued},       // unplaced — always fresh
		{ID: 3, Host: CloudInstanceHost(cloudInstanceID), Status: StatusQueued, CloudInstanceID: &cloudInstanceID}, // rental — always fresh
		{ID: 4, Host: "cool30", Status: StatusRunning},                                                             // inventory — no match
	}

	filtered := FilterByFreshStatus(jobs, []string{"studio"})
	wantIDs := []int64{1, 2, 3}
	if len(filtered) != len(wantIDs) {
		t.Fatalf("expected %d jobs, got %d", len(wantIDs), len(filtered))
	}
	for i, id := range wantIDs {
		if filtered[i].ID != id {
			t.Errorf("filtered[%d].ID = %d, want %d", i, filtered[i].ID, id)
		}
	}
}

func TestFilterByHostMatchesOnlyRequestedHosts(t *testing.T) {
	cloudInstanceID := int64(17)
	jobs := []*Job{
		{ID: 1, Host: "studio", Status: StatusQueued},
		{ID: 2, Host: "", Status: StatusQueued},
		{ID: 3, Host: CloudInstanceHost(cloudInstanceID), Status: StatusQueued, CloudInstanceID: &cloudInstanceID},
		{ID: 4, Host: "cool30", Status: StatusRunning},
	}

	filtered := FilterByHost(jobs, []string{"studio"})
	if len(filtered) != 1 {
		t.Fatalf("expected 1 studio job, got %d", len(filtered))
	}
	if filtered[0].ID != 1 {
		t.Fatalf("filtered job ID = %d, want 1", filtered[0].ID)
	}
}

func TestJobTargetKindAndDisplay(t *testing.T) {
	cloudInstanceID := int64(17)
	tests := []struct {
		name          string
		job           *Job
		wantKind      JobTargetKind
		wantDisplay   string
		wantRental    bool
		wantInventory bool
	}{
		{
			name:          "inventory host",
			job:           &Job{Host: "studio", Status: StatusQueued},
			wantKind:      JobTargetInventoryHost,
			wantDisplay:   "studio",
			wantRental:    false,
			wantInventory: true,
		},
		{
			name:          "unplaced",
			job:           &Job{Host: "", Status: StatusQueued},
			wantKind:      JobTargetUnplaced,
			wantDisplay:   "(unplaced)",
			wantRental:    false,
			wantInventory: false,
		},
		{
			name:          "rental instance id is canonical",
			job:           &Job{CloudInstanceID: &cloudInstanceID, Status: StatusQueued},
			wantKind:      JobTargetRentalInstance,
			wantDisplay:   "rental:17",
			wantRental:    true,
			wantInventory: false,
		},
		{
			name:          "legacy synthetic rental host still reads as rental",
			job:           &Job{Host: CloudInstanceHost(cloudInstanceID), Status: StatusQueued},
			wantKind:      JobTargetRentalInstance,
			wantDisplay:   CloudInstanceHost(cloudInstanceID),
			wantRental:    true,
			wantInventory: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.job.TargetKind(); got != tt.wantKind {
				t.Fatalf("TargetKind() = %q, want %q", got, tt.wantKind)
			}
			if got := tt.job.TargetDisplay(); got != tt.wantDisplay {
				t.Fatalf("TargetDisplay() = %q, want %q", got, tt.wantDisplay)
			}
			if got := tt.job.IsRentalJob(); got != tt.wantRental {
				t.Fatalf("IsRentalJob() = %v, want %v", got, tt.wantRental)
			}
			if got := tt.job.HasInventoryHost(); got != tt.wantInventory {
				t.Fatalf("HasInventoryHost() = %v, want %v", got, tt.wantInventory)
			}
		})
	}
}

func TestListJobsWithMaxAgeForHostsIncludesNonInventoryJobs(t *testing.T) {
	database := SetupTestDB(t)

	studioJobID, err := RecordQueuedWithGPU(database, "studio", "/tmp/project-studio", "python train.py", "studio", "")
	if err != nil {
		t.Fatalf("record studio job: %v", err)
	}
	cool30JobID, err := RecordQueuedWithGPU(database, "cool30", "/tmp/project-cool30", "python eval.py", "cool30", "")
	if err != nil {
		t.Fatalf("record other host job: %v", err)
	}
	unplacedJobID, err := RecordQueuedWithGPU(database, "", "/tmp/project-unplaced", "python wait.py", "unplaced", "")
	if err != nil {
		t.Fatalf("record unplaced job: %v", err)
	}
	cloudJobID, err := RecordQueuedWithGPU(database, CloudInstanceHost(17), "/tmp/project-cloud", "python cloud.py", "cloud", "")
	if err != nil {
		t.Fatalf("record cloud job: %v", err)
	}
	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	if err := SetJobCloudInstanceID(database, cloudJobID, instanceID); err != nil {
		t.Fatalf("set cloud instance on job: %v", err)
	}

	// Freshness filter: studio is "fresh", cool30 is not.
	// Unplaced and cloud jobs should pass through regardless.
	jobs, err := ListJobsWithMaxAgeForHosts(database, "", []string{"studio"}, 0, 0, nil, "")
	if err != nil {
		t.Fatalf("ListJobsWithMaxAgeForHosts: %v", err)
	}

	gotIDs := make(map[int64]bool)
	for _, j := range jobs {
		gotIDs[j.ID] = true
	}

	if !gotIDs[studioJobID] {
		t.Errorf("studio job %d missing (inventory, host matches)", studioJobID)
	}
	if !gotIDs[unplacedJobID] {
		t.Errorf("unplaced job %d missing (should always pass)", unplacedJobID)
	}
	if gotIDs[cool30JobID] {
		t.Errorf("cool30 job %d present (inventory, host not in fresh set)", cool30JobID)
	}
	// Cloud job (cloud:17) passes SQL via cloud_instance_id IS NOT NULL;
	// in-memory FilterByFreshStatus also passes it via TargetKind().
}

func TestListUniqueActiveHostsIncludesDeferredOpHosts(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "unplaced", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := AddDeferredOperation(database, "cool30", OpRemoveQueued, jobID, "", ""); err != nil {
		t.Fatalf("add deferred op: %v", err)
	}

	hosts, err := ListUniqueActiveHosts(database)
	if err != nil {
		t.Fatalf("ListUniqueActiveHosts: %v", err)
	}
	if len(hosts) != 1 || hosts[0] != "cool30" {
		t.Fatalf("hosts = %v, want [cool30]", hosts)
	}
}

func TestJobCloudAttempts(t *testing.T) {
	database := SetupTestDB(t)

	// Create an unplaced job and a cloud instance
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("record unplaced: %v", err)
	}

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}

	// Associate job with instance (should create attempt)
	if err := SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("set job cloud instance: %v", err)
	}

	// Verify attempt was created
	attempts, err := GetJobCloudAttempts(database, jobID)
	if err != nil {
		t.Fatalf("get attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(attempts))
	}
	if attempts[0].JobID != jobID {
		t.Errorf("attempt JobID = %d, want %d", attempts[0].JobID, jobID)
	}
	if attempts[0].CloudInstanceID != instanceID {
		t.Errorf("attempt CloudInstanceID = %d, want %d", attempts[0].CloudInstanceID, instanceID)
	}
	if attempts[0].EndedAt != nil {
		t.Error("attempt should not have ended yet")
	}

	// Close the attempt
	if err := CloseJobCloudAttempt(database, jobID, "failed"); err != nil {
		t.Fatalf("close attempt: %v", err)
	}

	attempts, err = GetJobCloudAttempts(database, jobID)
	if err != nil {
		t.Fatalf("get attempts after close: %v", err)
	}
	if attempts[0].EndedAt == nil {
		t.Error("attempt should have ended")
	}
	if attempts[0].Outcome != "failed" {
		t.Errorf("attempt outcome = %q, want %q", attempts[0].Outcome, "failed")
	}

	// Create second instance and associate (simulates retry)
	instanceID2, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create second instance: %v", err)
	}
	if err := SetJobCloudInstanceID(database, jobID, instanceID2); err != nil {
		t.Fatalf("set job cloud instance 2: %v", err)
	}

	// Should now have 2 attempts
	attempts, err = GetJobCloudAttempts(database, jobID)
	if err != nil {
		t.Fatalf("get attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(attempts))
	}
}

func TestJobCloudAttemptTriggersRejectSecondOpenAttempt(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance(first): %v", err)
	}
	instanceID2, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance(second): %v", err)
	}

	if err := InsertJobCloudAttempt(database, jobID, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt(first): %v", err)
	}
	err = InsertJobCloudAttempt(database, jobID, instanceID2)
	if err == nil {
		t.Fatal("expected second open attempt to fail")
	}
	if !strings.Contains(err.Error(), "open cloud attempt") {
		t.Fatalf("second open attempt error = %v, want open-attempt guard", err)
	}
}

func TestJobCloudAttemptTriggersSyncJobAssignment(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	if err := InsertJobCloudAttempt(database, jobID, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID(after open): %v", err)
	}
	if job.CloudInstanceID == nil || *job.CloudInstanceID != instanceID {
		t.Fatalf("cloud_instance_id = %v, want %d", job.CloudInstanceID, instanceID)
	}
	if job.Host != "" {
		t.Fatalf("host = %q, want empty", job.Host)
	}

	if err := CloseJobCloudAttempt(database, jobID, AttemptOutcomeFailed); err != nil {
		t.Fatalf("CloseJobCloudAttempt: %v", err)
	}

	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID(after close): %v", err)
	}
	// After the schema refactor, the view reads cloud_instance_id from the latest
	// attempt. A closed attempt retains its cloud_instance_id (it ran on that instance).
	// The old behavior (clearing cloud_instance_id after close) is no longer needed
	// because the view shows the attempt's association, not the job's placement.
	if job.CloudInstanceID == nil || *job.CloudInstanceID != instanceID {
		t.Fatalf("cloud_instance_id = %v, want %d (retained from closed attempt)", job.CloudInstanceID, instanceID)
	}
}

func TestJobCloudAttemptTriggersPreventClearingLiveAssignment(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}
	if err := SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobCloudInstanceID: %v", err)
	}

	_, err = database.Exec(`UPDATE jobs SET cloud_instance_id = NULL WHERE id = ?`, jobID)
	if err == nil {
		t.Fatal("expected clearing live cloud assignment to fail")
	}
	if !strings.Contains(err.Error(), "live cloud attempt") {
		t.Fatalf("clear cloud_instance_id error = %v, want live-attempt guard", err)
	}
}

func TestJobStatusChecksRejectInvalidValues(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	if _, err := database.Exec(`UPDATE jobs SET status = ? WHERE id = ?`, "bogus", jobID); err == nil {
		t.Fatal("expected invalid jobs.status update to fail")
	}
	if _, err := database.Exec(`UPDATE jobs SET pending_status = ? WHERE id = ?`, "bogus", jobID); err == nil {
		t.Fatal("expected invalid jobs.pending_status update to fail")
	}
}

func TestCloudInstanceStatusChecksRejectInvalidValues(t *testing.T) {
	database := SetupTestDB(t)

	if _, err := database.Exec(
		`INSERT INTO cloud_instances (status, provider, created_at) VALUES (?, ?, ?)`,
		"bogus", "vastai", time.Now().Unix(),
	); err == nil {
		t.Fatal("expected invalid cloud_instances.status insert to fail")
	}
}

func TestCloudInstanceTerminationReasonChecksRejectInvalidValues(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	if _, err := database.Exec(`UPDATE cloud_instances SET termination_reason = ? WHERE id = ?`, "bogus", instanceID); err == nil {
		t.Fatal("expected invalid cloud_instances.termination_reason update to fail")
	}
}

func TestCampaignStatusChecksRejectInvalidValues(t *testing.T) {
	database := SetupTestDB(t)

	if _, err := database.Exec(
		`INSERT INTO campaigns (status, created_at) VALUES (?, ?)`,
		"bogus", time.Now().Unix(),
	); err == nil {
		t.Fatal("expected invalid campaigns.status insert to fail")
	}
}

func TestJobCloudAttemptShapeChecksRejectInvalidValues(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO job_cloud_attempts (job_id, cloud_instance_id, started_at, outcome) VALUES (?, ?, ?, ?)`,
		jobID, instanceID, time.Now().Unix(), AttemptOutcomeCompleted,
	); err == nil {
		t.Fatal("expected open attempt with outcome to fail")
	}
	if _, err := database.Exec(
		`INSERT INTO job_cloud_attempts (job_id, cloud_instance_id, started_at, ended_at) VALUES (?, ?, ?, ?)`,
		jobID, instanceID, time.Now().Unix(), time.Now().Unix(),
	); err == nil {
		t.Fatal("expected closed attempt without outcome to fail")
	}

	if err := InsertJobCloudAttempt(database, jobID, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_cloud_attempts SET outcome = ? WHERE job_id = ? AND ended_at IS NULL`, AttemptOutcomeFailed, jobID); err == nil {
		t.Fatal("expected shape-preserving update to open attempt to fail")
	}
	if _, err := database.Exec(`UPDATE job_cloud_attempts SET ended_at = ? WHERE job_id = ? AND ended_at IS NULL`, time.Now().Unix(), jobID); err == nil {
		t.Fatal("expected closing attempt without outcome to fail")
	}
	if _, err := database.Exec(`UPDATE job_cloud_attempts SET ended_at = ?, outcome = ? WHERE job_id = ? AND ended_at IS NULL`, time.Now().Unix(), "bogus", jobID); err == nil {
		t.Fatal("expected invalid outcome update to fail")
	}
}

func TestLatestRunOwnershipTriggerRejectsMismatchedJobs(t *testing.T) {
	t.Skip("job_runs archival removed")
}

func TestRunOwnedArtifactAndTimeseriesTriggersRejectMismatchedJobs(t *testing.T) {
	t.Skip("job_runs archival removed")
}

func TestPlacementTriggerRejectsNonEmptyHostForCloudJobs(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	if _, err := database.Exec(`UPDATE jobs SET cloud_instance_id = ?, host = ? WHERE id = ?`, instanceID, "studio", jobID); err == nil {
		t.Fatal("expected mixed host/cloud placement update to fail")
	}
}

func TestInitSchemaRepairsLegacyCloudPlacementAndLiveAttempts(t *testing.T) {
	database := setupRawInitSchemaDB(t)

	if _, err := database.Exec(`
		CREATE TABLE jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			host TEXT NOT NULL,
			session_name TEXT,
			working_dir TEXT NOT NULL,
			command TEXT NOT NULL,
			description TEXT,
			start_time INTEGER,
			end_time INTEGER,
			exit_code INTEGER,
			status TEXT NOT NULL DEFAULT 'running',
			tombstoned INTEGER NOT NULL DEFAULT 0,
			cloud_instance_id INTEGER
		);
		CREATE TABLE cloud_instances (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			campaign_id INTEGER,
			status TEXT NOT NULL DEFAULT 'planned',
			provider TEXT NOT NULL DEFAULT 'vastai',
			gpu_spec TEXT,
			gpu_class TEXT,
			gpu_mem_gb INTEGER,
			vastai_instance_id TEXT,
			max_spend_cents INTEGER,
			max_time_seconds INTEGER,
			actual_spend_cents INTEGER,
			created_at INTEGER NOT NULL
			,launched_at INTEGER,
			ended_at INTEGER
		);
		CREATE TABLE job_cloud_attempts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id INTEGER NOT NULL,
			cloud_instance_id INTEGER NOT NULL,
			started_at INTEGER NOT NULL,
			ended_at INTEGER,
			outcome TEXT
		);`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	if _, err := database.Exec(`INSERT INTO cloud_instances (id, status, provider, created_at) VALUES (1, ?, 'vastai', ?)`, CloudInstanceStatusRunning, time.Now().Unix()); err != nil {
		t.Fatalf("insert cloud instance: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO jobs (id, host, working_dir, command, status, tombstoned, cloud_instance_id) VALUES (1, ?, '/tmp/project', 'python train.py', ?, 0, NULL)`, CloudInstanceHost(1), StatusQueued); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO job_cloud_attempts (job_id, cloud_instance_id, started_at) VALUES (1, 1, ?)`, time.Now().Unix()); err != nil {
		t.Fatalf("insert job_cloud_attempts: %v", err)
	}

	if err := initSchema(database); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	job, err := GetJobByID(database, 1)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Host != "" {
		t.Fatalf("host = %q, want empty after repair", job.Host)
	}
	if job.CloudInstanceID == nil || *job.CloudInstanceID != 1 {
		t.Fatalf("cloud_instance_id = %v, want 1 after repair", job.CloudInstanceID)
	}
}

func TestInitSchemaFailsOnInvalidLatestRunOwnership(t *testing.T) {
	database := setupRawInitSchemaDB(t)

	if _, err := database.Exec(`
		CREATE TABLE jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			host TEXT NOT NULL,
			session_name TEXT,
			working_dir TEXT NOT NULL,
			command TEXT NOT NULL,
			description TEXT,
			start_time INTEGER,
			end_time INTEGER,
			exit_code INTEGER,
			status TEXT NOT NULL DEFAULT 'running',
			tombstoned INTEGER NOT NULL DEFAULT 0,
			latest_run_id INTEGER
		);
		CREATE TABLE job_runs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id INTEGER NOT NULL,
			archived_at INTEGER NOT NULL,
			archive_reason TEXT NOT NULL,
			status TEXT NOT NULL,
			host TEXT NOT NULL,
			working_dir TEXT NOT NULL,
			command TEXT NOT NULL
		);`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	if _, err := database.Exec(`INSERT INTO jobs (id, host, working_dir, command, status, tombstoned, latest_run_id) VALUES (1, '', '/tmp/a', 'echo a', ?, 0, 10)`, StatusQueued); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO job_runs (id, job_id, archived_at, archive_reason, status, host, working_dir, command) VALUES (10, 2, ?, 'legacy', ?, '', '/tmp/b', 'echo b')`, time.Now().Unix(), StatusQueued); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	err := initSchema(database)
	if err == nil {
		t.Fatal("expected initSchema to fail on invalid latest_run ownership")
	}
	if !strings.Contains(err.Error(), "latest_run_id ownership") {
		t.Fatalf("initSchema error = %v, want latest_run ownership diagnostic", err)
	}
}

func TestResetCloudInstanceJobsClosesAttempts(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("record unplaced: %v", err)
	}

	instanceID, err := CreateCloudInstance(database, &CloudInstance{
		Status:   CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}

	// Assign host then associate with instance
	if _, err := AssignJobHost(database, jobID, "cloud"); err != nil {
		t.Fatalf("assign host: %v", err)
	}
	if err := SetJobCloudInstanceID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}

	// Reset should close attempts with "orphaned"
	count, err := ResetCloudInstanceJobs(database, instanceID, "orphaned")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if count != 1 {
		t.Errorf("reset count = %d, want 1", count)
	}

	attempts, err := GetJobCloudAttempts(database, jobID)
	if err != nil {
		t.Fatalf("get attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(attempts))
	}
	if attempts[0].Outcome != "orphaned" {
		t.Errorf("attempt outcome = %q, want %q", attempts[0].Outcome, "orphaned")
	}
	if attempts[0].EndedAt == nil {
		t.Error("attempt should have ended")
	}
}

func TestSetJobEnvVars(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	env := []string{"FOO=bar", "CUDA_VISIBLE_DEVICES=0"}
	if err := SetJobEnvVars(database, jobID, env); err != nil {
		t.Fatalf("set env vars: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got, want := job.EnvVars, env; len(got) != len(want) {
		t.Fatalf("env vars len = %d, want %d", len(got), len(want))
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("env vars[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	}

	if err := SetJobEnvVars(database, jobID, nil); err != nil {
		t.Fatalf("clear env vars: %v", err)
	}
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job after clear: %v", err)
	}
	if len(job.EnvVars) != 0 {
		t.Fatalf("expected env vars cleared, got %v", job.EnvVars)
	}
}

func TestSetJobCPUAllotment(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "host", "/tmp", "echo ok", "desc")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	allotment := 40
	if err := SetJobCPUAllotment(database, jobID, &allotment); err != nil {
		t.Fatalf("set cpu allotment: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.CPUAllotment == nil || *job.CPUAllotment != allotment {
		t.Fatalf("expected CPU allotment %d, got %#v", allotment, job.CPUAllotment)
	}

	if err := SetJobCPUAllotment(database, jobID, nil); err != nil {
		t.Fatalf("clear cpu allotment: %v", err)
	}

	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.CPUAllotment != nil {
		t.Fatalf("expected CPU allotment cleared, got %#v", job.CPUAllotment)
	}
}

func TestSetJobGPUMemGB(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "host", "/tmp", "echo ok", "desc")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	gpuMem := 20
	if err := SetJobGPUMemGB(database, jobID, &gpuMem); err != nil {
		t.Fatalf("set gpu mem: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB == nil || *job.GPUMemGB != gpuMem {
		t.Fatalf("expected GPU mem %d, got %#v", gpuMem, job.GPUMemGB)
	}

	if err := SetJobGPUMemGB(database, jobID, nil); err != nil {
		t.Fatalf("clear gpu mem: %v", err)
	}

	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.GPUMemGB != nil {
		t.Fatalf("expected GPU mem cleared, got %#v", job.GPUMemGB)
	}
}

func TestSetJobTags(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	tags := []string{"exp-012", "processed", "exp-012", " "}
	if err := SetJobTags(database, jobID, tags); err != nil {
		t.Fatalf("set tags: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	want := []string{"exp-012", "processed"}
	if got := job.Tags; len(got) != len(want) {
		t.Fatalf("tags len = %d, want %d", len(got), len(want))
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("tags[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	}

	if err := SetJobTags(database, jobID, nil); err != nil {
		t.Fatalf("clear tags: %v", err)
	}
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job after clear: %v", err)
	}
	if len(job.Tags) != 0 {
		t.Fatalf("expected tags cleared, got %v", job.Tags)
	}
}

func TestSetJobTagsCanonicalizesLegacyPlacementAliases(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	if err := SetJobTags(database, jobID, []string{TagCloudLegacy, TagOnPremLegacy, TagCloudLegacy}); err == nil {
		t.Fatalf("expected conflicting placement tags to fail")
	}

	if err := SetJobTags(database, jobID, []string{TagCloudLegacy, "exp-012"}); err != nil {
		t.Fatalf("set tags: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	want := []string{TagRental, "exp-012"}
	if got := job.Tags; len(got) != len(want) {
		t.Fatalf("tags len = %d, want %d (%v)", len(got), len(want), got)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("tags[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	}
}

func TestAddAndRemoveJobTagSupportLegacyPlacementAliases(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	if err := AddJobTag(database, jobID, TagCloudLegacy); err != nil {
		t.Fatalf("add legacy rental tag: %v", err)
	}
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if !job.HasTag(TagRental) {
		t.Fatalf("expected canonical rental tag, got %v", job.Tags)
	}

	if err := RemoveJobTag(database, jobID, TagRental); err != nil {
		t.Fatalf("remove canonical rental tag: %v", err)
	}
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job after remove: %v", err)
	}
	if len(job.Tags) != 0 {
		t.Fatalf("expected tags cleared, got %v", job.Tags)
	}
}

func TestFilterJobsByExcludedTags(t *testing.T) {
	jobWithTag := &Job{ID: 1, Tags: []string{"exp-012"}}
	jobWithoutTag := &Job{ID: 2, Tags: []string{"other"}}
	jobs := []*Job{jobWithTag, jobWithoutTag}

	filtered := FilterJobsByExcludedTags(jobs, []string{"exp-012"})
	if len(filtered) != 1 || filtered[0].ID != 2 {
		t.Fatalf("expected job 2 only, got %+v", filtered)
	}
}

func TestHostSyncTracking(t *testing.T) {
	database := SetupTestDB(t)

	now := time.Unix(1_700_000_000, 0)
	old := now.Add(-72 * time.Hour)

	if err := RecordHostSync(database, "host-beta", now); err != nil {
		t.Fatalf("RecordHostSync host-beta: %v", err)
	}
	if err := RecordHostSync(database, "host-alpha", old); err != nil {
		t.Fatalf("RecordHostSync host-alpha: %v", err)
	}

	times, err := LoadHostSyncTimes(database)
	if err != nil {
		t.Fatalf("LoadHostSyncTimes: %v", err)
	}
	if len(times) != 2 {
		t.Fatalf("expected 2 host sync entries, got %d", len(times))
	}
	if got := times["host-beta"]; !got.Equal(now) {
		t.Fatalf("host-beta last sync = %v, want %v", got, now)
	}
	if got := times["host-alpha"]; !got.Equal(old) {
		t.Fatalf("host-alpha last sync = %v, want %v", got, old)
	}

	recentHosts, err := ListHostsSyncedSince(database, now.Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("ListHostsSyncedSince: %v", err)
	}
	if len(recentHosts) != 1 || recentHosts[0] != "host-beta" {
		t.Fatalf("expected [host-beta], got %v", recentHosts)
	}
}

func TestQueuedTransitionsClearRunMetadata(t *testing.T) {
	database := SetupTestDB(t)

	now := time.Now().Unix()
	exitCode := 1

	makeRunningJob := func(status string) int64 {
		jobID, err := RecordQueued(database, "hostA", "/tmp", "echo test", "test")
		if err != nil {
			t.Fatalf("record queued: %v", err)
		}
		if _, err := database.Exec(
			`UPDATE jobs SET status = ?, session_name = ?, start_time = ?, end_time = ?, exit_code = ?, error_message = ? WHERE id = ?`,
			status, fmt.Sprintf("rj-%d", jobID), now-100, now, exitCode, "boom", jobID,
		); err != nil {
			t.Fatalf("seed job fields: %v", err)
		}
		return jobID
	}

	jobID := makeRunningJob(StatusRunning)
	if err := UpdateJobRunningToQueued(database, jobID); err != nil {
		t.Fatalf("UpdateJobRunningToQueued: %v", err)
	}
	updated, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if updated.Status != StatusQueued {
		t.Fatalf("expected queued status, got %s", updated.Status)
	}
	if updated.SessionName != "" || updated.StartTime != 0 || updated.EndTime != nil || updated.ExitCode != nil || updated.ErrorMessage != "" {
		t.Fatalf("expected run metadata cleared after running->queued, got session=%q start=%d end=%v exit=%v err=%q",
			updated.SessionName, updated.StartTime, updated.EndTime, updated.ExitCode, updated.ErrorMessage)
	}

	jobID = makeRunningJob(StatusStarting)
	if err := UpdateJobStartingToQueued(database, jobID); err != nil {
		t.Fatalf("UpdateJobStartingToQueued: %v", err)
	}
	updated, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if updated.Status != StatusQueued {
		t.Fatalf("expected queued status, got %s", updated.Status)
	}
	if updated.SessionName != "" || updated.StartTime != 0 || updated.EndTime != nil || updated.ExitCode != nil || updated.ErrorMessage != "" {
		t.Fatalf("expected run metadata cleared after starting->queued, got session=%q start=%d end=%v exit=%v err=%q",
			updated.SessionName, updated.StartTime, updated.EndTime, updated.ExitCode, updated.ErrorMessage)
	}

	jobID = makeRunningJob(StatusRunning)
	if err := MarkQueuedByID(database, jobID); err != nil {
		t.Fatalf("MarkQueuedByID: %v", err)
	}
	updated, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if updated.Status != StatusQueued {
		t.Fatalf("expected queued status, got %s", updated.Status)
	}
	if updated.SessionName != "" || updated.StartTime != 0 || updated.EndTime != nil || updated.ExitCode != nil || updated.ErrorMessage != "" {
		t.Fatalf("expected run metadata cleared after mark queued, got session=%q start=%d end=%v exit=%v err=%q",
			updated.SessionName, updated.StartTime, updated.EndTime, updated.ExitCode, updated.ErrorMessage)
	}

	jobID = makeRunningJob(StatusRunning)
	if err := ClearPendingAndUpdateStatus(database, jobID, StatusQueued); err != nil {
		t.Fatalf("ClearPendingAndUpdateStatus queued: %v", err)
	}
	updated, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if updated.Status != StatusQueued {
		t.Fatalf("expected queued status, got %s", updated.Status)
	}
	if updated.SessionName != "" || updated.StartTime != 0 || updated.EndTime != nil || updated.ExitCode != nil || updated.ErrorMessage != "" {
		t.Fatalf("expected run metadata cleared after clear pending queued, got session=%q start=%d end=%v exit=%v err=%q",
			updated.SessionName, updated.StartTime, updated.EndTime, updated.ExitCode, updated.ErrorMessage)
	}

	jobID = makeRunningJob(StatusRunning)
	if err := ClearPendingAndUpdateStatus(database, jobID, StatusDraft); err != nil {
		t.Fatalf("ClearPendingAndUpdateStatus draft: %v", err)
	}
	updated, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if updated.Status != StatusDraft {
		t.Fatalf("expected draft status, got %s", updated.Status)
	}
	if updated.SessionName != "" || updated.StartTime != 0 || updated.EndTime != nil || updated.ExitCode != nil || updated.ErrorMessage != "" {
		t.Fatalf("expected run metadata cleared after clear pending draft, got session=%q start=%d end=%v exit=%v err=%q",
			updated.SessionName, updated.StartTime, updated.EndTime, updated.ExitCode, updated.ErrorMessage)
	}
}

func TestMarkRunningFromTerminalArchivesPreviousRun(t *testing.T) {
	t.Skip("job_runs archival removed; job_attempts tracks history")
}

func TestRequeueByIDArchivesPreviousRun(t *testing.T) {
	t.Skip("job_runs archival removed; job_attempts tracks history")
}

func TestUpdateQueuedToRunningCreatesLatestRun(t *testing.T) {
	t.Skip("job_runs archival removed; job_attempts tracks history via LatestRunID → attempt ID")
}

func TestGetGPU_DatabaseFieldTakesPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		gpu     string
		command string
		want    string
	}{
		{
			name:    "database field only",
			gpu:     "3",
			command: "python train.py",
			want:    "3",
		},
		{
			name:    "database field overrides command",
			gpu:     "5",
			command: "env CUDA_VISIBLE_DEVICES=0 python train.py",
			want:    "5",
		},
		{
			name:    "empty database field falls back to command",
			gpu:     "",
			command: "env CUDA_VISIBLE_DEVICES=2 python train.py",
			want:    "2",
		},
		{
			name:    "multiple GPUs in database field",
			gpu:     "0,1,2",
			command: "python train.py",
			want:    "0,1,2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{GPU: tt.gpu, Command: tt.command}
			got := job.GetGPU()
			if got != tt.want {
				t.Errorf("GetGPU() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetGPU_EnvVars(t *testing.T) {
	tests := []struct {
		name    string
		gpu     string
		envVars []string
		command string
		want    string
	}{
		{
			name:    "env vars only",
			envVars: []string{"CUDA_VISIBLE_DEVICES=1"},
			command: "python train.py",
			want:    "1",
		},
		{
			name:    "env vars multiple GPUs",
			envVars: []string{"CUDA_VISIBLE_DEVICES=0,1"},
			command: "python train.py",
			want:    "0,1",
		},
		{
			name:    "env vars override database field",
			gpu:     "3",
			envVars: []string{"CUDA_VISIBLE_DEVICES=1"},
			command: "python train.py",
			want:    "1",
		},
		{
			name:    "env vars override command",
			envVars: []string{"CUDA_VISIBLE_DEVICES=1"},
			command: "CUDA_VISIBLE_DEVICES=0 python train.py",
			want:    "1",
		},
		{
			name:    "env vars with other vars",
			envVars: []string{"FOO=bar", "CUDA_VISIBLE_DEVICES=2", "BAZ=qux"},
			command: "python train.py",
			want:    "2",
		},
		{
			name:    "env vars without CUDA",
			envVars: []string{"FOO=bar"},
			command: "python train.py",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{GPU: tt.gpu, EnvVars: tt.envVars, Command: tt.command}
			got := job.GetGPU()
			if got != tt.want {
				t.Errorf("GetGPU() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeCommand(t *testing.T) {
	tests := []struct {
		name       string
		command    string
		wantDir    string
		wantCmd    string
		wantEnvLen int
	}{
		{
			name:       "simple command",
			command:    "python train.py",
			wantDir:    "",
			wantCmd:    "python train.py",
			wantEnvLen: 0,
		},
		{
			name:       "cd prefix",
			command:    "cd /foo/bar && python train.py",
			wantDir:    "/foo/bar",
			wantCmd:    "python train.py",
			wantEnvLen: 0,
		},
		{
			name:       "env prefix",
			command:    "env CUDA_VISIBLE_DEVICES=0 python train.py",
			wantDir:    "",
			wantCmd:    "python train.py",
			wantEnvLen: 1,
		},
		{
			name:       "export prefix",
			command:    "export TMPDIR=/tmp && python train.py",
			wantDir:    "",
			wantCmd:    "python train.py",
			wantEnvLen: 1,
		},
		{
			name:       "cd then export",
			command:    "cd /foo && export TMPDIR=/tmp && python train.py",
			wantDir:    "/foo",
			wantCmd:    "python train.py",
			wantEnvLen: 1,
		},
		{
			name:       "cd then env",
			command:    "cd /foo && env CUDA_VISIBLE_DEVICES=0 python train.py",
			wantDir:    "/foo",
			wantCmd:    "python train.py",
			wantEnvLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotDir, gotCmd, gotEnv := NormalizeCommand(tt.command)
			if gotDir != tt.wantDir {
				t.Errorf("NormalizeCommand() dir = %q, want %q", gotDir, tt.wantDir)
			}
			if gotCmd != tt.wantCmd {
				t.Errorf("NormalizeCommand() cmd = %q, want %q", gotCmd, tt.wantCmd)
			}
			if len(gotEnv) != tt.wantEnvLen {
				t.Errorf("NormalizeCommand() env len = %d, want %d (env: %v)", len(gotEnv), tt.wantEnvLen, gotEnv)
			}
		})
	}
}

func TestListJobsPendingReconciliation(t *testing.T) {
	db := setupTestDB(t)

	// Create jobs with different states
	job1ID, _ := RecordQueued(db, "host1", "/tmp", "cmd1", "no pending")
	job2ID, _ := RecordQueued(db, "host1", "/tmp", "cmd2", "has pending")
	job3ID, _ := RecordQueued(db, "host2", "/tmp", "cmd3", "different host")

	// Set pending status on job2 and job3
	SetPendingStatus(db, job2ID, StatusKilled)
	SetPendingStatus(db, job3ID, StatusCanceled)

	// List pending jobs for host1
	jobs, err := ListJobsPendingReconciliation(db, "host1")
	if err != nil {
		t.Fatalf("ListJobsPendingReconciliation failed: %v", err)
	}

	// Should only return job2 (host1 with pending status)
	if len(jobs) != 1 {
		t.Errorf("expected 1 job, got %d", len(jobs))
	}
	if len(jobs) > 0 && jobs[0].ID != job2ID {
		t.Errorf("expected job ID %d, got %d", job2ID, jobs[0].ID)
	}

	// List pending jobs for host2
	jobs, err = ListJobsPendingReconciliation(db, "host2")
	if err != nil {
		t.Fatalf("ListJobsPendingReconciliation failed: %v", err)
	}
	if len(jobs) != 1 {
		t.Errorf("expected 1 job for host2, got %d", len(jobs))
	}

	// Job without pending status should not be included
	_, _ = job1ID, job3ID // silence unused variable warnings
}

// TestSyncFunctionsUpdateLastSyncedStatus verifies that all sync operations
// update both status and last_synced_status together. This prevents bugs where
// a job's status shows one thing but last_synced_status shows another.
func TestEffectiveStatus(t *testing.T) {
	tests := []struct {
		name          string
		status        string
		pendingStatus *string
		host          string
		want          string
	}{
		{
			name:          "no pending status returns actual status",
			status:        StatusRunning,
			pendingStatus: nil,
			host:          "host-a",
			want:          StatusRunning,
		},
		{
			name:          "pending status overrides actual status",
			status:        StatusRunning,
			pendingStatus: ptr(StatusKilled),
			host:          "host-a",
			want:          StatusKilled,
		},
		{
			name:          "queued with pending canceled",
			status:        StatusQueued,
			pendingStatus: ptr(StatusCanceled),
			host:          "host-a",
			want:          StatusCanceled,
		},
		{
			name:          "running with pending paused",
			status:        StatusRunning,
			pendingStatus: ptr(StatusPaused),
			host:          "host-a",
			want:          StatusPaused,
		},
		{
			name:          "paused with pending running (resume)",
			status:        StatusPaused,
			pendingStatus: ptr(StatusRunning),
			host:          "host-a",
			want:          StatusRunning,
		},
		{
			name:          "hostless running is treated as queued",
			status:        StatusRunning,
			pendingStatus: nil,
			host:          "",
			want:          StatusQueued,
		},
		{
			name:          "hostless starting is treated as queued",
			status:        StatusStarting,
			pendingStatus: nil,
			host:          "",
			want:          StatusQueued,
		},
		{
			name:          "hostless paused is treated as queued",
			status:        StatusPaused,
			pendingStatus: nil,
			host:          "",
			want:          StatusQueued,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{
				Status:        tt.status,
				PendingStatus: tt.pendingStatus,
				Host:          tt.host,
			}
			got := job.EffectiveStatus()
			if got != tt.want {
				t.Errorf("EffectiveStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ptr returns a pointer to the given string (helper for tests)
func ptr(s string) *string {
	return &s
}

func TestSyncFunctionsUpdateLastSyncedStatus(t *testing.T) {
	db := setupTestDB(t)

	t.Run("RecordCompletionByID updates last_synced_status", func(t *testing.T) {
		jobID, _ := RecordQueued(db, "host1", "/tmp", "cmd1", "test")
		MarkQueuedJobRunning(db, jobID) // First transition to running

		err := RecordCompletionByID(db, jobID, 0, time.Now().Unix())
		if err != nil {
			t.Fatalf("RecordCompletionByID failed: %v", err)
		}

		job, _ := GetJobByID(db, jobID)
		if job.Status != StatusCompleted {
			t.Errorf("status = %s, want %s", job.Status, StatusCompleted)
		}
		if job.LastSyncedStatus != StatusCompleted {
			t.Errorf("last_synced_status = %s, want %s", job.LastSyncedStatus, StatusCompleted)
		}
	})

	t.Run("RecordCompletionByID accepts failed status", func(t *testing.T) {
		// This tests the race condition recovery: a job was marked failed locally
		// but the status file on remote shows it actually completed
		jobID, _ := RecordQueued(db, "host1", "/tmp", "cmd-failed", "test")
		MarkQueuedJobRunning(db, jobID)
		MarkDeadByID(db, jobID) // Mark as failed (simulating race condition)

		// Now sync finds status file showing completion
		err := RecordCompletionByID(db, jobID, 0, time.Now().Unix())
		if err != nil {
			t.Fatalf("RecordCompletionByID from failed status should not error: %v", err)
		}

		job, _ := GetJobByID(db, jobID)
		if job.Status != StatusCompleted {
			t.Errorf("status = %s, want %s", job.Status, StatusCompleted)
		}
	})

	t.Run("RecordCompletionByID accepts dead status", func(t *testing.T) {
		// Similar to above but starting from dead status
		jobID, _ := RecordQueued(db, "host1", "/tmp", "cmd-dead", "test")
		MarkQueuedJobRunning(db, jobID)
		// Simulate marking as dead (using direct SQL since there's no MarkDead function that sets StatusDead)
		db.Exec("UPDATE jobs SET status = ? WHERE id = ?", StatusDead, jobID)

		err := RecordCompletionByID(db, jobID, 0, time.Now().Unix())
		if err != nil {
			t.Fatalf("RecordCompletionByID from dead status should not error: %v", err)
		}

		job, _ := GetJobByID(db, jobID)
		if job.Status != StatusCompleted {
			t.Errorf("status = %s, want %s", job.Status, StatusCompleted)
		}
	})

	t.Run("MarkDeadByID updates last_synced_status", func(t *testing.T) {
		jobID, _ := RecordQueued(db, "host1", "/tmp", "cmd2", "test")
		MarkQueuedJobRunning(db, jobID)

		err := MarkDeadByID(db, jobID)
		if err != nil {
			t.Fatalf("MarkDeadByID failed: %v", err)
		}

		job, _ := GetJobByID(db, jobID)
		if job.Status != StatusFailed {
			t.Errorf("status = %s, want %s", job.Status, StatusFailed)
		}
		if job.LastSyncedStatus != StatusFailed {
			t.Errorf("last_synced_status = %s, want %s", job.LastSyncedStatus, StatusFailed)
		}
	})

	t.Run("MarkQueuedJobRunning updates last_synced_status", func(t *testing.T) {
		jobID, _ := RecordQueued(db, "host1", "/tmp", "cmd3", "test")

		err := MarkQueuedJobRunning(db, jobID)
		if err != nil {
			t.Fatalf("MarkQueuedJobRunning failed: %v", err)
		}

		job, _ := GetJobByID(db, jobID)
		if job.Status != StatusRunning {
			t.Errorf("status = %s, want %s", job.Status, StatusRunning)
		}
		if job.LastSyncedStatus != StatusRunning {
			t.Errorf("last_synced_status = %s, want %s", job.LastSyncedStatus, StatusRunning)
		}
	})

	t.Run("MarkQueuedByID updates last_synced_status", func(t *testing.T) {
		jobID, _ := RecordQueued(db, "host1", "/tmp", "cmd4", "test")
		MarkQueuedJobRunning(db, jobID) // First make it running

		err := MarkQueuedByID(db, jobID) // Then reset to queued (sync found it still queued)
		if err != nil {
			t.Fatalf("MarkQueuedByID failed: %v", err)
		}

		job, _ := GetJobByID(db, jobID)
		if job.Status != StatusQueued {
			t.Errorf("status = %s, want %s", job.Status, StatusQueued)
		}
		if job.LastSyncedStatus != StatusQueued {
			t.Errorf("last_synced_status = %s, want %s", job.LastSyncedStatus, StatusQueued)
		}
	})
}

func TestSetJobInputsOutputs(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "hostA", "/tmp", "python train.py", "test")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	// Set inputs
	inputs := []string{"hf:meta-llama/Llama-3-8B", "hf-dataset:allenai/dolma"}
	if err := SetJobInputs(database, jobID, inputs); err != nil {
		t.Fatalf("set inputs: %v", err)
	}

	// Set outputs
	outputs := []string{"checkpoint:llama-ft-v1"}
	if err := SetJobOutputs(database, jobID, outputs); err != nil {
		t.Fatalf("set outputs: %v", err)
	}

	// Read back
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	if len(job.Inputs) != 2 {
		t.Fatalf("inputs len = %d, want 2", len(job.Inputs))
	}
	if job.Inputs[0] != "hf:meta-llama/Llama-3-8B" {
		t.Errorf("inputs[0] = %q, want %q", job.Inputs[0], "hf:meta-llama/Llama-3-8B")
	}
	if job.Inputs[1] != "hf-dataset:allenai/dolma" {
		t.Errorf("inputs[1] = %q, want %q", job.Inputs[1], "hf-dataset:allenai/dolma")
	}

	if len(job.Outputs) != 1 {
		t.Fatalf("outputs len = %d, want 1", len(job.Outputs))
	}
	if job.Outputs[0] != "checkpoint:llama-ft-v1" {
		t.Errorf("outputs[0] = %q, want %q", job.Outputs[0], "checkpoint:llama-ft-v1")
	}

	// Clear inputs
	if err := SetJobInputs(database, jobID, nil); err != nil {
		t.Fatalf("clear inputs: %v", err)
	}
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job after clear: %v", err)
	}
	if len(job.Inputs) != 0 {
		t.Fatalf("expected inputs cleared, got %v", job.Inputs)
	}
	if len(job.Outputs) != 1 {
		t.Fatalf("outputs should still be set, got %v", job.Outputs)
	}
}

func TestRecordQueuedSetsLastSyncedStatus(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	if job.LastSyncedStatus != "" {
		t.Errorf("last_synced_status = %q, want empty", job.LastSyncedStatus)
	}
}

func TestUpdateLastSyncedStatusSkipsTerminalJobs(t *testing.T) {
	database := SetupTestDB(t)

	for _, terminalStatus := range []string{StatusFailed, StatusDead, StatusKilled, StatusCanceled} {
		t.Run(terminalStatus, func(t *testing.T) {
			jobID, err := RecordQueued(database, "hostA", "/tmp", "echo test", "test")
			if err != nil {
				t.Fatalf("record queued: %v", err)
			}

			// Set job to terminal status (trigger syncs to job_attempts)
			_, err = database.Exec("UPDATE jobs SET status = ?, last_synced_status = ? WHERE id = ?",
				terminalStatus, terminalStatus, jobID)
			if err != nil {
				t.Fatalf("set terminal status: %v", err)
			}

			// Attempt to overwrite last_synced_status
			err = UpdateLastSyncedStatus(database, jobID, StatusQueued)
			if err != nil {
				t.Fatalf("UpdateLastSyncedStatus: %v", err)
			}

			job, err := GetJobByID(database, jobID)
			if err != nil {
				t.Fatalf("get job: %v", err)
			}

			if job.LastSyncedStatus != terminalStatus {
				t.Errorf("last_synced_status = %q, want %q (should not be overwritten)", job.LastSyncedStatus, terminalStatus)
			}
		})
	}
}

func TestJobInputsOutputsEmptyByDefault(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "hostA", "/tmp", "echo test", "test")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	if job.Inputs != nil {
		t.Errorf("expected nil inputs for new job, got %v", job.Inputs)
	}
	if job.Outputs != nil {
		t.Errorf("expected nil outputs for new job, got %v", job.Outputs)
	}
}
