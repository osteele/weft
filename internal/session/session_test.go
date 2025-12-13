package session

import (
	"strings"
	"testing"
)

func TestTmuxSessionName(t *testing.T) {
	tests := []struct {
		jobID    int64
		expected string
	}{
		{1, "rj-1"},
		{42, "rj-42"},
		{12345, "rj-12345"},
	}

	for _, tt := range tests {
		got := TmuxSessionName(tt.jobID)
		if got != tt.expected {
			t.Errorf("TmuxSessionName(%d) = %q, want %q", tt.jobID, got, tt.expected)
		}
	}
}

func TestFileBasename(t *testing.T) {
	// Test with a known timestamp: 2024-12-12 21:03:00 UTC
	startTime := int64(1734040980)
	got := FileBasename(42, startTime)
	// The format depends on local timezone, so just check it starts with job ID
	if got[:3] != "42-" {
		t.Errorf("FileBasename(42, %d) = %q, want to start with '42-'", startTime, got)
	}
	if len(got) != 18 { // "42-20241212-210300"
		t.Errorf("FileBasename(42, %d) = %q, unexpected length %d", startTime, got, len(got))
	}
}

func TestLogFile(t *testing.T) {
	startTime := int64(1734040980)
	got := LogFile(42, startTime)
	// Should contain the log dir and end with .log
	if got[:len(LogDir)] != LogDir {
		t.Errorf("LogFile should start with LogDir")
	}
	if got[len(got)-4:] != ".log" {
		t.Errorf("LogFile should end with .log")
	}
}

func TestStatusFile(t *testing.T) {
	startTime := int64(1734040980)
	got := StatusFile(42, startTime)
	if got[len(got)-7:] != ".status" {
		t.Errorf("StatusFile should end with .status")
	}
}

func TestMetadataFile(t *testing.T) {
	startTime := int64(1734040980)
	got := MetadataFile(42, startTime)
	if got[len(got)-5:] != ".meta" {
		t.Errorf("MetadataFile should end with .meta")
	}
}

func TestLegacyLogFile(t *testing.T) {
	got := LegacyLogFile("train-gpt2")
	want := "/tmp/tmux-train-gpt2.log"
	if got != want {
		t.Errorf("LegacyLogFile(%q) = %q, want %q", "train-gpt2", got, want)
	}
}

func TestParseMetadata(t *testing.T) {
	content := `job_id=42
working_dir=/mnt/code/LM2
command=python train.py
start_time=1234567890
host=cool30
description=Training run`

	result := ParseMetadata(content)

	if result["job_id"] != "42" {
		t.Errorf("job_id = %q, want %q", result["job_id"], "42")
	}
	if result["working_dir"] != "/mnt/code/LM2" {
		t.Errorf("working_dir = %q, want %q", result["working_dir"], "/mnt/code/LM2")
	}
	if result["command"] != "python train.py" {
		t.Errorf("command = %q, want %q", result["command"], "python train.py")
	}
	if result["host"] != "cool30" {
		t.Errorf("host = %q, want %q", result["host"], "cool30")
	}
	if result["description"] != "Training run" {
		t.Errorf("description = %q, want %q", result["description"], "Training run")
	}
}

func TestFormatMetadata(t *testing.T) {
	content := FormatMetadata(42, "/mnt/code", "python train.py", "cool30", "Test job", 1234567890)

	expected := map[string]string{
		"job_id":      "42",
		"working_dir": "/mnt/code",
		"command":     "python train.py",
		"host":        "cool30",
		"description": "Test job",
		"start_time":  "1234567890",
	}

	parsed := ParseMetadata(content)
	for key, want := range expected {
		if got := parsed[key]; got != want {
			t.Errorf("parsed[%q] = %q, want %q", key, got, want)
		}
	}
}

// TestBuildWrapperCommand_TildeExpansion verifies that tilde paths are NOT quoted,
// which would prevent shell expansion. This is a critical test to prevent regressions.
func TestBuildWrapperCommand_TildeExpansion(t *testing.T) {
	params := WrapperCommandParams{
		JobID:      42,
		WorkingDir: "~/code/project",
		Command:    "python train.py",
		LogFile:    "~/.cache/remote-jobs/logs/42.log",
		StatusFile: "~/.cache/remote-jobs/logs/42.status",
		PidFile:    "~/.cache/remote-jobs/logs/42.pid",
		NotifyCmd:  "",
	}

	cmd := BuildWrapperCommand(params)

	// CRITICAL: Tilde paths must NOT be single-quoted
	// Single quotes prevent tilde expansion in bash
	badPatterns := []struct {
		pattern string
		desc    string
	}{
		{"'~/code/project'", "working directory with tilde should not be single-quoted"},
		{"'~/.cache/remote-jobs/logs/42.log'", "log file with tilde should not be single-quoted"},
		{"'~/.cache/remote-jobs/logs/42.status'", "status file with tilde should not be single-quoted"},
		{"'~/.cache/remote-jobs/logs/42.pid'", "pid file with tilde should not be single-quoted"},
	}

	for _, bp := range badPatterns {
		if strings.Contains(cmd, bp.pattern) {
			t.Errorf("BuildWrapperCommand: %s\nFound quoted pattern: %s\nCommand: %s", bp.desc, bp.pattern, cmd)
		}
	}

	// Verify the paths ARE present (unquoted)
	goodPatterns := []struct {
		pattern string
		desc    string
	}{
		{"cd ~/code/project", "working directory should appear unquoted after cd"},
		{"> ~/.cache/remote-jobs/logs/42.log", "log file should appear unquoted"},
		{">> ~/.cache/remote-jobs/logs/42.log", "log file should appear unquoted in append"},
		{"> ~/.cache/remote-jobs/logs/42.status", "status file should appear unquoted"},
		{"> ~/.cache/remote-jobs/logs/42.pid", "pid file should appear unquoted"},
	}

	for _, gp := range goodPatterns {
		if !strings.Contains(cmd, gp.pattern) {
			t.Errorf("BuildWrapperCommand: %s\nExpected pattern not found: %s\nCommand: %s", gp.desc, gp.pattern, cmd)
		}
	}
}

// TestBuildWrapperCommand_AbsolutePaths verifies that absolute paths work correctly
func TestBuildWrapperCommand_AbsolutePaths(t *testing.T) {
	params := WrapperCommandParams{
		JobID:      99,
		WorkingDir: "/mnt/data/project",
		Command:    "make build",
		LogFile:    "/tmp/job-99.log",
		StatusFile: "/tmp/job-99.status",
		PidFile:    "/tmp/job-99.pid",
		NotifyCmd:  "",
	}

	cmd := BuildWrapperCommand(params)

	// Absolute paths should appear in the command
	if !strings.Contains(cmd, "cd /mnt/data/project") {
		t.Errorf("BuildWrapperCommand: working directory not found\nCommand: %s", cmd)
	}
	if !strings.Contains(cmd, "/tmp/job-99.log") {
		t.Errorf("BuildWrapperCommand: log file not found\nCommand: %s", cmd)
	}
}

// TestBuildWrapperCommand_NotifyCmd verifies that notification command is properly appended
func TestBuildWrapperCommand_NotifyCmd(t *testing.T) {
	params := WrapperCommandParams{
		JobID:      42,
		WorkingDir: "~/code/project",
		Command:    "python train.py",
		LogFile:    "~/.cache/remote-jobs/logs/42.log",
		StatusFile: "~/.cache/remote-jobs/logs/42.status",
		PidFile:    "~/.cache/remote-jobs/logs/42.pid",
		NotifyCmd:  "; notify-slack.sh rj-42 $EXIT_CODE cool30",
	}

	cmd := BuildWrapperCommand(params)

	// Notify command should be appended at the end
	if !strings.HasSuffix(cmd, "; notify-slack.sh rj-42 $EXIT_CODE cool30") {
		t.Errorf("BuildWrapperCommand: notify command not properly appended\nCommand: %s", cmd)
	}

	// $EXIT_CODE should NOT be escaped (must expand at runtime)
	if strings.Contains(cmd, "\\$EXIT_CODE") {
		t.Errorf("BuildWrapperCommand: $EXIT_CODE should not be escaped\nCommand: %s", cmd)
	}
}

// TestBuildWrapperCommand_CommandPreserved verifies that the user command is preserved correctly
func TestBuildWrapperCommand_CommandPreserved(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{"simple", "python train.py"},
		{"with args", "python train.py --epochs 100 --lr 0.001"},
		{"with pipe", "cat data.txt | grep error"},
		{"with redirect", "python train.py > output.txt 2>&1"},
		{"with env var", "CUDA_VISIBLE_DEVICES=0 python train.py"},
		{"with semicolon", "echo start; python train.py; echo done"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := WrapperCommandParams{
				JobID:      1,
				WorkingDir: "~/code",
				Command:    tt.command,
				LogFile:    "~/.cache/remote-jobs/logs/1.log",
				StatusFile: "~/.cache/remote-jobs/logs/1.status",
				PidFile:    "~/.cache/remote-jobs/logs/1.pid",
			}

			cmd := BuildWrapperCommand(params)

			// The command should appear in the wrapper (in the subshell)
			if !strings.Contains(cmd, tt.command) {
				t.Errorf("BuildWrapperCommand: command not preserved\nExpected: %s\nCommand: %s", tt.command, cmd)
			}
		})
	}
}

// TestBuildWrapperCommand_PidCapture verifies PID is captured correctly
func TestBuildWrapperCommand_PidCapture(t *testing.T) {
	params := WrapperCommandParams{
		JobID:      42,
		WorkingDir: "~/code",
		Command:    "python train.py",
		LogFile:    "~/.cache/remote-jobs/logs/42.log",
		StatusFile: "~/.cache/remote-jobs/logs/42.status",
		PidFile:    "~/.cache/remote-jobs/logs/42.pid",
	}

	cmd := BuildWrapperCommand(params)

	// Must capture PID with background + $!
	if !strings.Contains(cmd, "CMD_PID=$!") {
		t.Errorf("BuildWrapperCommand: PID capture not found\nCommand: %s", cmd)
	}

	// Must write PID to file
	if !strings.Contains(cmd, "echo $CMD_PID > ~/.cache/remote-jobs/logs/42.pid") {
		t.Errorf("BuildWrapperCommand: PID file write not found\nCommand: %s", cmd)
	}

	// Must wait for the process
	if !strings.Contains(cmd, "wait $CMD_PID") {
		t.Errorf("BuildWrapperCommand: wait for PID not found\nCommand: %s", cmd)
	}
}

// TestBuildWrapperCommand_ExitCodeCapture verifies exit code is captured correctly
func TestBuildWrapperCommand_ExitCodeCapture(t *testing.T) {
	params := WrapperCommandParams{
		JobID:      42,
		WorkingDir: "~/code",
		Command:    "python train.py",
		LogFile:    "~/.cache/remote-jobs/logs/42.log",
		StatusFile: "~/.cache/remote-jobs/logs/42.status",
		PidFile:    "~/.cache/remote-jobs/logs/42.pid",
	}

	cmd := BuildWrapperCommand(params)

	// Must capture exit code from PIPESTATUS (due to tee pipe)
	if !strings.Contains(cmd, "EXIT_CODE=${PIPESTATUS[0]}") {
		t.Errorf("BuildWrapperCommand: PIPESTATUS capture not found\nCommand: %s", cmd)
	}

	// Must write exit code to status file
	if !strings.Contains(cmd, "echo $EXIT_CODE > ~/.cache/remote-jobs/logs/42.status") {
		t.Errorf("BuildWrapperCommand: exit code file write not found\nCommand: %s", cmd)
	}
}
