package session

import "testing"

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
