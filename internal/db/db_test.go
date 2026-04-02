package db

import (
	"database/sql"
	"errors"
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

			// Create an unplaced job, give it an attempt, then fail it.
			jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
			if err != nil {
				t.Fatalf("record unplaced: %v", err)
			}
			// Create an attempt and mark it failed.
			if _, err := CreateAttempt(database, jobID, "", nil, StatusQueued); err != nil {
				t.Fatalf("create attempt: %v", err)
			}
			if err := CloseAttempt(database, jobID, StatusFailed, nil, 1000); err != nil {
				t.Fatalf("close attempt: %v", err)
			}
			// Clear requested_status so the view derives status from the attempt.
			if _, err := database.Exec(`UPDATE jobs SET requested_status = NULL WHERE id = ?`, jobID); err != nil {
				t.Fatalf("clear requested_status: %v", err)
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

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "cloud assigned", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
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

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "open attempt", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	jobs, err := ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("ListUnplacedJobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected no unplaced jobs, got %d", len(jobs))
	}
}

func TestListUnplacedJobsIncludesJobsResetFromTerminalInstances(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "stale open attempt", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	if _, err := NormalizeTerminalLaunchJobs(database, instanceID); err != nil {
		t.Fatalf("NormalizeTerminalLaunchJobs: %v", err)
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

func TestJobStatusViewTargetKind(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	// Job with cloud instance should be rental_instance
	var targetKind string
	err = database.QueryRow(`SELECT effective_target_kind FROM job_status WHERE id = ?`, jobID).Scan(&targetKind)
	if err != nil {
		t.Fatalf("QueryRow(job_status): %v", err)
	}
	if targetKind != string(JobTargetRentalInstance) {
		t.Fatalf("effective_target_kind = %q, want %q", targetKind, JobTargetRentalInstance)
	}

	// Unplaced job
	unplacedID, err := RecordQueuedWithGPU(database, "", "/tmp/project2", "python eval.py", "unplaced", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU unplaced: %v", err)
	}
	err = database.QueryRow(`SELECT effective_target_kind FROM job_status WHERE id = ?`, unplacedID).Scan(&targetKind)
	if err != nil {
		t.Fatalf("QueryRow(job_status unplaced): %v", err)
	}
	if targetKind != string(JobTargetUnplaced) {
		t.Fatalf("effective_target_kind = %q, want %q", targetKind, JobTargetUnplaced)
	}
}

func TestJobStatusViewTargetKind_PlannedLaunchIsUnplaced(t *testing.T) {
	database := SetupTestDB(t)

	// A launch that never progressed past "planned" should not claim jobs.
	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusPlanned,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	var targetKind string
	err = database.QueryRow(`SELECT effective_target_kind FROM job_status WHERE id = ?`, jobID).Scan(&targetKind)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if targetKind != string(JobTargetUnplaced) {
		t.Fatalf("effective_target_kind = %q, want %q (planned launch should not claim jobs)", targetKind, JobTargetUnplaced)
	}

	// The job should appear in ListUnplacedJobs.
	jobs, err := ListUnplacedJobs(database)
	if err != nil {
		t.Fatalf("ListUnplacedJobs: %v", err)
	}
	found := false
	for _, j := range jobs {
		if j.ID == jobID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("job %d not found in ListUnplacedJobs (launch is planned)", jobID)
	}
}

func TestJobStatusViewTargetKind_FailedLaunchIsUnplaced(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	// Transition launch to failed — job should become unplaced in the view.
	if err := UpdateLaunchStatus(database, instanceID, LaunchStatusFailed, TerminationReasonInfraFailure, "test failure"); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}

	var targetKind string
	err = database.QueryRow(`SELECT effective_target_kind FROM job_status WHERE id = ?`, jobID).Scan(&targetKind)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if targetKind != string(JobTargetUnplaced) {
		t.Fatalf("effective_target_kind = %q, want %q (failed launch should not claim jobs)", targetKind, JobTargetUnplaced)
	}
}

func TestSetJobLaunchID_RejectsActiveClaimReturnsError(t *testing.T) {
	database := SetupTestDB(t)

	// Create a running launch and assign a job to it.
	instanceA, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch A: %v", err)
	}
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, instanceA); err != nil {
		t.Fatalf("SetJobLaunchID (first claim): %v", err)
	}

	// A second launch trying to claim the same job should get ErrJobAlreadyClaimed.
	instanceB, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusPlanned,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch B: %v", err)
	}
	err = SetJobLaunchID(database, jobID, instanceB)
	if !errors.Is(err, ErrJobAlreadyClaimed) {
		t.Fatalf("expected ErrJobAlreadyClaimed, got: %v", err)
	}
}

func TestSetJobLaunchID_AllowsReclaimFromStaleLaunch(t *testing.T) {
	database := SetupTestDB(t)

	// Create a planned (stale) launch and assign a job to it.
	instanceA, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusPlanned,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch A: %v", err)
	}
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, instanceA); err != nil {
		t.Fatalf("SetJobLaunchID (first): %v", err)
	}

	// A second launch should be able to reclaim since instanceA is planned (not active).
	instanceB, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusPlanned,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch B: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, instanceB); err != nil {
		t.Fatalf("SetJobLaunchID (reclaim from stale): %v", err)
	}

	// Verify the job is now assigned to instanceB.
	var launchID sql.NullInt64
	err = database.QueryRow(`SELECT launch_id FROM job_attempts WHERE job_id = ? AND end_time IS NULL ORDER BY attempt_number DESC LIMIT 1`, jobID).Scan(&launchID)
	if err != nil {
		t.Fatalf("QueryRow: %v", err)
	}
	if !launchID.Valid || launchID.Int64 != instanceB {
		t.Fatalf("launch_id = %v, want %d", launchID, instanceB)
	}
}

func TestListUnplacedJobsIncludesHostlessQueued(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "hostless queued", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
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

func TestHasTagHostConflict(t *testing.T) {
	database := SetupTestDB(t)

	// Queued on inventory host with rental tag → conflict
	jobID, err := RecordQueuedWithGPU(database, "host-beta", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	if err := SetJobTags(database, jobID, []string{TagRental}); err != nil {
		t.Fatalf("set tags: %v", err)
	}
	job, _ := GetJobByID(database, jobID)
	if !job.HasTagHostConflict() {
		t.Error("expected HasTagHostConflict=true for rental-tagged job on inventory host")
	}

	// Queued on inventory host without rental tag → no conflict
	jobID2, err := RecordQueuedWithGPU(database, "host-beta", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	job2, _ := GetJobByID(database, jobID2)
	if job2.HasTagHostConflict() {
		t.Error("expected HasTagHostConflict=false for job without rental tag")
	}

	// Unplaced job with rental tag → no conflict (already unplaced)
	jobID3, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	if err := SetJobTags(database, jobID3, []string{TagRental}); err != nil {
		t.Fatalf("set tags: %v", err)
	}
	job3, _ := GetJobByID(database, jobID3)
	if job3.HasTagHostConflict() {
		t.Error("expected HasTagHostConflict=false for unplaced job")
	}
}

func TestResetJobToUnplacedSetsReason(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("record queued: %v", err)
	}
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	// Place the job on the launch via SetJobLaunchID (creates the attempt).
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	// Mark the attempt as running.
	if err := UpdateAttemptRunning(database, jobID); err != nil {
		t.Fatalf("UpdateAttemptRunning: %v", err)
	}

	if err := ResetJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("ResetJobToUnplaced: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got := strings.Join(job.PlacementReasons, "\n"); got != fmt.Sprintf("cloud instance %d unavailable; job reset to unplaced queue", launchID) {
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
	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	if err := SetJobLaunchID(database, cloudJobID, instanceID); err != nil {
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
		if job.LaunchID != nil {
			t.Fatalf("unexpected cloud job in on-prem list: %+v", job)
		}
		if job.Host == "" {
			t.Fatalf("unexpected unplaced job in on-prem list: %+v", job)
		}
	}
}

func TestListActiveCloudJobsIncludesOpenLiveAttemptJobs(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "cloud", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
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
	if jobs[0].LaunchID == nil || *jobs[0].LaunchID != instanceID {
		t.Fatalf("cloud_instance_id = %v, want %d", jobs[0].LaunchID, instanceID)
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
	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, StatusDraft, draftID); err != nil {
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
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, StatusKilled, now-40, killedID); err != nil {
		t.Fatalf("mark killed: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, StatusKilled, killedID); err != nil {
		t.Fatalf("set killed requested_status: %v", err)
	}

	canceledID, err := RecordQueued(database, "cool30", "/tmp/project-epsilon", "python canceled.py", "canceled")
	if err != nil {
		t.Fatalf("record canceled: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ?, end_time = ? WHERE job_id = ? AND end_time IS NULL`, StatusCanceled, now-50, canceledID); err != nil {
		t.Fatalf("mark canceled: %v", err)
	}
	if _, err := database.Exec(`UPDATE jobs SET requested_status = ? WHERE id = ?`, StatusCanceled, canceledID); err != nil {
		t.Fatalf("set canceled requested_status: %v", err)
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
		{ID: 1, Host: "studio", Status: StatusQueued},                                                // inventory — matches
		{ID: 2, Host: "", Status: StatusQueued},                                                      // unplaced — always fresh
		{ID: 3, Host: LaunchHost(cloudInstanceID), Status: StatusQueued, LaunchID: &cloudInstanceID}, // rental — always fresh
		{ID: 4, Host: "cool30", Status: StatusRunning},                                               // inventory — no match
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
		{ID: 3, Host: LaunchHost(cloudInstanceID), Status: StatusQueued, LaunchID: &cloudInstanceID},
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
			job:           &Job{LaunchID: &cloudInstanceID, Status: StatusQueued},
			wantKind:      JobTargetRentalInstance,
			wantDisplay:   "rental:17",
			wantRental:    true,
			wantInventory: false,
		},
		{
			name:          "legacy synthetic rental host still reads as rental",
			job:           &Job{Host: LaunchHost(cloudInstanceID), Status: StatusQueued},
			wantKind:      JobTargetRentalInstance,
			wantDisplay:   LaunchHost(cloudInstanceID),
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
	cloudJobID, err := RecordQueuedWithGPU(database, LaunchHost(17), "/tmp/project-cloud", "python cloud.py", "cloud", "")
	if err != nil {
		t.Fatalf("record cloud job: %v", err)
	}
	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}
	if err := SetJobLaunchID(database, cloudJobID, instanceID); err != nil {
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

func TestLaunchAttempts(t *testing.T) {
	database := SetupTestDB(t)

	// Create an unplaced job and a cloud instance
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("record unplaced: %v", err)
	}

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create cloud instance: %v", err)
	}

	// Associate job with instance (should create attempt)
	if err := SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set job cloud instance: %v", err)
	}

	// Verify attempt was created
	attempts, err := GetLaunchAttempts(database, jobID)
	if err != nil {
		t.Fatalf("get attempts: %v", err)
	}
	if len(attempts) != 1 {
		t.Fatalf("expected 1 attempt, got %d", len(attempts))
	}
	if attempts[0].JobID != jobID {
		t.Errorf("attempt JobID = %d, want %d", attempts[0].JobID, jobID)
	}
	if attempts[0].LaunchID != instanceID {
		t.Errorf("attempt LaunchID = %d, want %d", attempts[0].LaunchID, instanceID)
	}
	if attempts[0].EndedAt != nil {
		t.Error("attempt should not have ended yet")
	}

	// Close the attempt (both cloud_outcome and end_time)
	if err := CloseLaunchAttempt(database, jobID, "failed"); err != nil {
		t.Fatalf("close cloud attempt: %v", err)
	}
	exitCode := 1
	if err := CloseAttempt(database, jobID, StatusFailed, &exitCode, time.Now().Unix()); err != nil {
		t.Fatalf("close attempt: %v", err)
	}

	attempts, err = GetLaunchAttempts(database, jobID)
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
	instanceID2, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create second instance: %v", err)
	}
	// Create a new attempt for the retry, then associate with the new instance
	if _, err := CreateAttempt(database, jobID, "", nil, StatusQueued); err != nil {
		t.Fatalf("create retry attempt: %v", err)
	}
	if err := SetJobLaunchID(database, jobID, instanceID2); err != nil {
		t.Fatalf("set job cloud instance 2: %v", err)
	}

	// Should now have 2 attempts
	attempts, err = GetLaunchAttempts(database, jobID)
	if err != nil {
		t.Fatalf("get attempts: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(attempts))
	}
}

func TestLaunchAttemptTriggersSyncJobAssignment(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	if err := SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID(after assignment): %v", err)
	}
	if job.LaunchID == nil || *job.LaunchID != instanceID {
		t.Fatalf("cloud_instance_id = %v, want %d", job.LaunchID, instanceID)
	}
	if job.Host != "" {
		t.Fatalf("host = %q, want empty", job.Host)
	}

	if err := CloseLaunchAttempt(database, jobID, AttemptOutcomeFailed); err != nil {
		t.Fatalf("CloseLaunchAttempt: %v", err)
	}

	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID(after close): %v", err)
	}
	// The view reads cloud_instance_id from the latest attempt. A closed attempt
	// retains its cloud_instance_id (it ran on that instance).
	if job.LaunchID == nil || *job.LaunchID != instanceID {
		t.Fatalf("cloud_instance_id = %v, want %d (retained from closed attempt)", job.LaunchID, instanceID)
	}
}

func TestJobStatusChecksRejectInvalidValues(t *testing.T) {
	// Status and pending_status constraints are now on job_attempts, not jobs.
	// The jobs table no longer has these columns.
	database := SetupTestDB(t)

	// Create a job with a host so it gets an attempt.
	jobID, err := RecordQueuedWithGPU(database, "cool30", "/tmp/project", "python train.py", "queued", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}

	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, "bogus", jobID); err == nil {
		t.Fatal("expected invalid job_attempts.status update to fail")
	}
}

func TestLaunchStatusChecksRejectInvalidValues(t *testing.T) {
	database := SetupTestDB(t)

	if _, err := database.Exec(
		`INSERT INTO launches (status, provider, created_at) VALUES (?, ?, ?)`,
		"bogus", "vastai", time.Now().Unix(),
	); err == nil {
		t.Fatal("expected invalid launches.status insert to fail")
	}
}

func TestLaunchTerminationReasonChecksRejectInvalidValues(t *testing.T) {
	database := SetupTestDB(t)

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	if _, err := database.Exec(`UPDATE launches SET termination_reason = ? WHERE id = ?`, "bogus", instanceID); err == nil {
		t.Fatal("expected invalid launches.termination_reason update to fail")
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

func TestPlacementTriggerRejectsNonEmptyHostForCloudJobs(t *testing.T) {
	// host and cloud_instance_id are no longer on the jobs table — they live
	// exclusively on job_attempts. The integrity trigger was removed.
	t.Skip("host/cloud_instance_id columns removed from jobs table")
}

func TestInitSchemaRepairsLegacyCloudPlacementAndLiveAttempts(t *testing.T) {
	t.Skip("legacy backfill removed")
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
			error_message TEXT,
			backend TEXT DEFAULT 'queue-runner',
			remote_id TEXT,
			remote_state TEXT,
			failure_reason TEXT,
			tombstoned INTEGER NOT NULL DEFAULT 0,
			cloud_instance_id INTEGER,
			queued_at INTEGER,
			last_synced_status TEXT,
			pending_status TEXT,
			pending_at INTEGER,
			cost REAL,
			vastai_instance_id INTEGER,
			placement_meta TEXT,
			job_metadata TEXT,
			observed_inputs TEXT,
			error_diagnosis TEXT
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

	if _, err := database.Exec(`INSERT INTO cloud_instances (id, status, provider, created_at) VALUES (1, ?, 'vastai', ?)`, LaunchStatusRunning, time.Now().Unix()); err != nil {
		t.Fatalf("insert cloud instance: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO jobs (id, host, working_dir, command, status, tombstoned, cloud_instance_id) VALUES (1, ?, '/tmp/project', 'python train.py', ?, 0, NULL)`, LaunchHost(1), StatusQueued); err != nil {
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
	// After migration, cloud_instance_id is backfilled from the legacy
	// job_cloud_attempts table into job_attempts.
	if job.LaunchID == nil || *job.LaunchID != 1 {
		t.Fatalf("cloud_instance_id = %v, want 1 after repair", job.LaunchID)
	}
}

func TestResetLaunchJobsClosesAttempts(t *testing.T) {
	database := SetupTestDB(t)

	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("record unplaced: %v", err)
	}

	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
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
	if err := SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}

	// Reset should close attempts with "orphaned"
	count, err := ResetLaunchJobs(database, instanceID, "orphaned")
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if count != 1 {
		t.Errorf("reset count = %d, want 1", count)
	}

	attempts, err := GetLaunchAttempts(database, jobID)
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
			`UPDATE job_attempts SET status = ?, session_name = ?, start_time = ?, end_time = ?, exit_code = ?, error_message = ? WHERE job_id = ? AND end_time IS NULL`,
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
		db.Exec("UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL", StatusDead, jobID)

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
			_, err = database.Exec("UPDATE job_attempts SET status = ?, last_synced_status = ? WHERE job_id = ? AND end_time IS NULL",
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

// TestCleanupStaleAttempts_SkipsJobsWithCompletedAttempt verifies that
// cleanupStaleAttempts does not create replacement attempts for jobs that
// already have a completed attempt, even if another attempt is open on a
// failed launch. This prevents the bug where a blank replacement attempt
// loses the launch association.
func TestCleanupStaleAttempts_SkipsJobsWithCompletedAttempt(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "weft-cleanup-terminal-*.db")
	if err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	cleanup := SetDBPath(tmpFile.Name())
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	// Create a failed launch.
	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create a job and assign it to the failed launch.
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatal(err)
	}

	// Simulate: a different mechanism already completed this job (e.g., R2 sync
	// from a different launch). Create a second completed attempt.
	if _, err := CreateAttempt(database, jobID, "vastai:12345", nil, StatusCompleted); err != nil {
		t.Fatal(err)
	}

	// Count attempts before re-open.
	var countBefore int
	database.QueryRow("SELECT COUNT(*) FROM job_attempts WHERE job_id = ?", jobID).Scan(&countBefore)

	database.Close()

	// Re-open triggers cleanupStaleAttempts.
	database, err = Open()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// Should NOT have created a new blank attempt.
	var countAfter int
	database.QueryRow("SELECT COUNT(*) FROM job_attempts WHERE job_id = ?", jobID).Scan(&countAfter)

	if countAfter != countBefore {
		t.Errorf("expected %d attempts (unchanged), got %d — cleanupStaleAttempts should skip jobs with a completed attempt", countBefore, countAfter)
	}

	// The job should still show as completed.
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusCompleted {
		t.Errorf("expected status=completed, got %q", job.Status)
	}
}

func TestStartupRepair_FixesCompletedCloudAttemptsMissingExitCode(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "weft-completed-repair-*.db")
	if err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	cleanup := SetDBPath(tmpFile.Name())
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	launchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatal(err)
	}
	if err := MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatal(err)
	}

	endTime := time.Now().Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts
		 SET status = ?, end_time = ?, exit_code = NULL, last_synced_status = ?, cloud_outcome = ?
		 WHERE job_id = ? AND end_time IS NULL`,
		StatusCompleted, endTime, StatusRunning, AttemptOutcomeCompleted, jobID,
	); err != nil {
		t.Fatalf("inject broken completed attempt: %v", err)
	}

	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusDead {
		t.Fatalf("pre-condition job status = %q, want %q", job.Status, StatusDead)
	}

	database.Close()

	database, err = Open()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusCompleted {
		t.Fatalf("job status after repair = %q, want %q", job.Status, StatusCompleted)
	}
	if job.ExitCode == nil || *job.ExitCode != 0 {
		t.Fatalf("job exit_code after repair = %v, want 0", job.ExitCode)
	}

	var lastSynced string
	if err := database.QueryRow(
		`SELECT COALESCE(last_synced_status, '') FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1`,
		jobID,
	).Scan(&lastSynced); err != nil {
		t.Fatalf("query repaired last_synced_status: %v", err)
	}
	if lastSynced != StatusCompleted {
		t.Fatalf("last_synced_status after repair = %q, want %q", lastSynced, StatusCompleted)
	}
}

// TestRepairOrphanedCompletedAttempts verifies that the repair migration
// correctly associates orphaned completed attempts with the correct launch
// by finding a sibling launch that ran other jobs from the same original launch.
func TestRepairOrphanedCompletedAttempts(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "weft-repair-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	cleanup := SetDBPath(tmpFile.Name())
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	// Create a failed launch (the original assignment that triggered cleanup).
	failedLaunchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
		GPUSpec:  "A100",
		GPUClass: "A100",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create a completed replacement launch with same GPU class.
	goodLaunchID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "A100",
		GPUClass: "A100",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create two jobs: jobA (orphaned) and jobB (properly completed on goodLaunch).
	// Both were originally assigned to failedLaunch.
	jobA, err := RecordQueuedWithGPU(database, "", "/tmp", "echo A", "test", "A100")
	if err != nil {
		t.Fatal(err)
	}
	jobB, err := RecordQueuedWithGPU(database, "", "/tmp", "echo B", "test", "A100")
	if err != nil {
		t.Fatal(err)
	}

	// Assign both jobs to the failed launch (creating attempt 2 on each).
	if err := SetJobLaunchID(database, jobA, failedLaunchID); err != nil {
		t.Fatal(err)
	}
	if err := SetJobLaunchID(database, jobB, failedLaunchID); err != nil {
		t.Fatal(err)
	}

	// Simulate cleanup: cancel the failed-launch attempts for jobA, create blank attempt 3.
	database.Exec(
		`UPDATE job_attempts SET status = 'canceled', end_time = 1000 WHERE job_id = ? AND launch_id = ?`,
		jobA, failedLaunchID,
	)
	if _, err := CreateAttempt(database, jobA, "", nil, StatusQueued); err != nil {
		t.Fatal(err)
	}
	// Mark jobA's blank attempt as completed (simulating R2 sync without launch_id).
	database.Exec(
		`UPDATE job_attempts SET status = 'completed', start_time = 200, end_time = 300, exit_code = 0
		 WHERE job_id = ? AND id = (SELECT MAX(id) FROM job_attempts WHERE job_id = ?)`,
		jobA, jobA,
	)

	// Simulate cleanup for jobB: cancel the failed-launch attempt, then properly
	// reassign to the good launch and complete.
	database.Exec(
		`UPDATE job_attempts SET status = 'canceled', end_time = 1000 WHERE job_id = ? AND launch_id = ?`,
		jobB, failedLaunchID,
	)
	if _, err := CreateAttempt(database, jobB, "", &goodLaunchID, StatusCompleted); err != nil {
		t.Fatal(err)
	}

	database.Close()

	// Re-open triggers startupRepair which includes repairOrphanedCompletedAttempts.
	database, err = Open()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	job, err := GetJobByID(database, jobA)
	if err != nil {
		t.Fatal(err)
	}

	if job.LaunchID == nil || *job.LaunchID != goodLaunchID {
		t.Errorf("expected launch_id=%d (good launch), got %v", goodLaunchID, job.LaunchID)
	}
	expectedHost := LaunchHost(goodLaunchID)
	if job.Host != expectedHost {
		t.Errorf("expected host=%s, got %q", expectedHost, job.Host)
	}
}

// TestCleanupStaleAttempts_SkipsCompletedInstances verifies that
// cleanupStaleAttempts does not create replacement attempts for jobs on
// completed instances. These jobs should be finalized by R2 result sync.
func TestCleanupStaleAttempts_SkipsCompletedInstances(t *testing.T) {
	// Use a file-backed DB so we can close and re-open (triggering cleanup).
	tmpFile, err := os.CreateTemp("", "weft-cleanup-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	tmpFile.Close()
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })

	cleanup := SetDBPath(tmpFile.Name())
	defer cleanup()

	database, err := Open()
	if err != nil {
		t.Fatal(err)
	}

	// Create a completed instance with an open (unfinalized) job attempt.
	instanceID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusCompleted,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp", "echo hi", "test", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatal(err)
	}

	// Record the original attempt ID.
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	origRunID := job.LatestRunID

	database.Close()

	// Re-open the DB, which triggers cleanupStaleAttempts.
	database, err = Open()
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	// The attempt should NOT have been replaced — still the same run ID.
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.LatestRunID == nil || origRunID == nil || *job.LatestRunID != *origRunID {
		t.Errorf("expected attempt to be preserved (run_id=%v), got run_id=%v", origRunID, job.LatestRunID)
	}
	if job.LaunchID == nil || *job.LaunchID != instanceID {
		t.Errorf("expected launch_id=%d, got %v", instanceID, job.LaunchID)
	}
}
