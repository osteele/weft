package session

import "testing"

func TestGenerateName(t *testing.T) {
	tests := []struct {
		command  string
		expected string
	}{
		{"python train.py", "python-train"},
		{"python script.py", "python-script"},
		{"with-gpu python train.py", "python-train"},
		{"env CUDA_VISIBLE_DEVICES=0 python train.py", "python-train"},
		{"python", "python"},
		{"python -m pytest", "python"},
		{"/usr/bin/python /path/to/script.py", "python-script"},
		{"just build", "just-build"},
		{"cargo test --release", "cargo-test"},
		{"", "job"},
	}

	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := GenerateName(tt.command)
			if got != tt.expected {
				t.Errorf("GenerateName(%q) = %q, want %q", tt.command, got, tt.expected)
			}
		})
	}
}

func TestGenerateNameMaxLength(t *testing.T) {
	// Test that very long names get truncated
	longCommand := "verylongprogramname verylongargumentname"
	name := GenerateName(longCommand)
	if len(name) > 30 {
		t.Errorf("GenerateName(%q) = %q (len=%d), want len <= 30", longCommand, name, len(name))
	}
}

func TestLogFile(t *testing.T) {
	got := LogFile("train-gpt2")
	want := "/tmp/tmux-train-gpt2.log"
	if got != want {
		t.Errorf("LogFile(%q) = %q, want %q", "train-gpt2", got, want)
	}
}

func TestStatusFile(t *testing.T) {
	got := StatusFile("train-gpt2")
	want := "/tmp/tmux-train-gpt2.status"
	if got != want {
		t.Errorf("StatusFile(%q) = %q, want %q", "train-gpt2", got, want)
	}
}

func TestMetadataFile(t *testing.T) {
	got := MetadataFile("train-gpt2")
	want := "/tmp/tmux-train-gpt2.meta"
	if got != want {
		t.Errorf("MetadataFile(%q) = %q, want %q", "train-gpt2", got, want)
	}
}

func TestParseMetadata(t *testing.T) {
	content := `working_dir=/mnt/code/LM2
command=python train.py
start_time=1234567890
host=cool30
description=Training run`

	result := ParseMetadata(content)

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
	content := FormatMetadata("/mnt/code", "python train.py", "cool30", "Test job", 1234567890)

	expected := map[string]string{
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
