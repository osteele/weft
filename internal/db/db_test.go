package db

import (
	"fmt"
	"testing"
	"time"
)

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

func TestAssignJobHost(t *testing.T) {
	database := SetupTestDB(t)

	// Create an unplaced job (queued with host="")
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "GPU training", "")
	if err != nil {
		t.Fatalf("record unplaced: %v", err)
	}

	// Assign a host
	assigned, err := AssignJobHost(database, jobID, "cool30")
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
	if job.Host != "cool30" {
		t.Errorf("Host = %q, want %q", job.Host, "cool30")
	}

	// Second assignment should be a no-op (host already set)
	assigned, err = AssignJobHost(database, jobID, "cool100")
	if err != nil {
		t.Fatalf("second assign: %v", err)
	}
	if assigned {
		t.Error("second assignment should fail (host already set)")
	}

	// Host should remain cool30
	job, err = GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job after second assign: %v", err)
	}
	if job.Host != "cool30" {
		t.Errorf("Host = %q, want %q (should not change)", job.Host, "cool30")
	}
}

func TestListUnplacedJobs(t *testing.T) {
	database := SetupTestDB(t)

	// Create an unplaced job (queued, host="")
	unplacedID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python train.py", "unplaced", "")
	if err != nil {
		t.Fatalf("record unplaced: %v", err)
	}

	// Create a placed job (queued, host="cool30")
	_, err = RecordQueuedWithGPU(database, "cool30", "/tmp/project", "echo hello", "placed", "")
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

	if err := RecordHostSync(database, "cool30", now); err != nil {
		t.Fatalf("RecordHostSync cool30: %v", err)
	}
	if err := RecordHostSync(database, "cool100", old); err != nil {
		t.Fatalf("RecordHostSync cool100: %v", err)
	}

	times, err := LoadHostSyncTimes(database)
	if err != nil {
		t.Fatalf("LoadHostSyncTimes: %v", err)
	}
	if len(times) != 2 {
		t.Fatalf("expected 2 host sync entries, got %d", len(times))
	}
	if got := times["cool30"]; !got.Equal(now) {
		t.Fatalf("cool30 last sync = %v, want %v", got, now)
	}
	if got := times["cool100"]; !got.Equal(old) {
		t.Fatalf("cool100 last sync = %v, want %v", got, old)
	}

	recentHosts, err := ListHostsSyncedSince(database, now.Add(-48*time.Hour))
	if err != nil {
		t.Fatalf("ListHostsSyncedSince: %v", err)
	}
	if len(recentHosts) != 1 || recentHosts[0] != "cool30" {
		t.Fatalf("expected [cool30], got %v", recentHosts)
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
		want          string
	}{
		{
			name:          "no pending status returns actual status",
			status:        StatusRunning,
			pendingStatus: nil,
			want:          StatusRunning,
		},
		{
			name:          "pending status overrides actual status",
			status:        StatusRunning,
			pendingStatus: ptr(StatusKilled),
			want:          StatusKilled,
		},
		{
			name:          "queued with pending canceled",
			status:        StatusQueued,
			pendingStatus: ptr(StatusCanceled),
			want:          StatusCanceled,
		},
		{
			name:          "running with pending paused",
			status:        StatusRunning,
			pendingStatus: ptr(StatusPaused),
			want:          StatusPaused,
		},
		{
			name:          "paused with pending running (resume)",
			status:        StatusPaused,
			pendingStatus: ptr(StatusRunning),
			want:          StatusRunning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &Job{
				Status:        tt.status,
				PendingStatus: tt.pendingStatus,
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

			// Set job to terminal status
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
