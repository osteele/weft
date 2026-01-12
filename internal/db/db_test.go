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

func TestSetJobEnvVars(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "hostA", "/tmp", "echo test", "test", "default")
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

func TestSetJobTags(t *testing.T) {
	database := SetupTestDB(t)
	jobID, err := RecordQueued(database, "hostA", "/tmp", "echo test", "test", "default")
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
		jobID, err := RecordQueued(database, "hostA", "/tmp", "echo test", "test", "default")
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
	if err := UpdateJobRunningToQueued(database, jobID, "default"); err != nil {
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
	if err := UpdateJobStartingToQueued(database, jobID, "default"); err != nil {
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
	job1ID, _ := RecordQueued(db, "host1", "/tmp", "cmd1", "no pending", "default")
	job2ID, _ := RecordQueued(db, "host1", "/tmp", "cmd2", "has pending", "default")
	job3ID, _ := RecordQueued(db, "host2", "/tmp", "cmd3", "different host", "default")

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
