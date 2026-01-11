package ops

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
)

func TestQueueJob_Success(t *testing.T) {
	database := db.SetupTestDB(t)
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "mkdir", Stdout: ""},
		{Contains: "flock", Stdout: ""},
	})

	params := QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp",
		Command:     "echo success",
		Description: "test job",
		QueueName:   "default",
	}
	result, err := QueueJob(database, params, DefaultOptions())
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true")
	}
	if result.Deferred {
		t.Error("expected Deferred to be false")
	}
	if result.Message != "Job 1 added to queue 'default'" {
		t.Errorf("unexpected message: %q", result.Message)
	}

	job, _ := db.GetJobByID(database, result.JobID)
	if job.Status != db.StatusQueued {
		t.Errorf("expected job status to be queued, got %s", job.Status)
	}
	if job.LastSyncedStatus != db.StatusQueued {
		t.Errorf("expected last synced status to be queued, got %q", job.LastSyncedStatus)
	}
}

func TestQueueJob_QuickTimeout(t *testing.T) {
	database := db.SetupTestDB(t)

	// Mock immediate connection failure (no sync attempted)
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "ssh: connect to host test-host port 22: Connection refused", 255
	})

	params := QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp",
		Command:     "echo success",
		Description: "test job",
	}
	result, err := QueueJob(database, params, ExecuteOptions{Timeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("QueueJob returned an unexpected error: %v", err)
	}

	if !result.Success {
		t.Error("expected Success to be true on deferred")
	}
	if !result.Deferred {
		t.Error("expected Deferred to be true")
	}

	job, _ := db.GetJobByID(database, result.JobID)
	if job.Status != db.StatusQueued {
		t.Errorf("expected job status to be queued, got %s", job.Status)
	}
	// LastSyncedStatus should be empty when deferred (not synced)
	if job.LastSyncedStatus != "" {
		t.Errorf("expected last synced status to be empty (not synced), got %q", job.LastSyncedStatus)
	}
}

// Note: QueueJob only makes a single SSH call (AppendQueueEntry),
// so there's no separate "SyncError" case distinct from QuickTimeout.
// The connection error case is already covered by TestQueueJob_QuickTimeout.

func TestQueueJob_ExtractsGPU(t *testing.T) {
	database := db.SetupTestDB(t)
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "mkdir", Stdout: ""},
		{Contains: "flock", Stdout: ""},
	})

	params := QueueJobParams{
		Host:        "test-host",
		WorkingDir:  "/tmp",
		Command:     "train",
		Description: "gpu job",
		EnvVars:     []string{"CUDA_VISIBLE_DEVICES=0,1", "OTHER_VAR=value"},
	}
	result, err := QueueJob(database, params, DefaultOptions())
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	job, _ := db.GetJobByID(database, result.JobID)
	if job.GPU != "0,1" {
		t.Errorf("expected GPU to be '0,1', got %q", job.GPU)
	}
}

func TestQueueJob_DefaultQueueName(t *testing.T) {
	database := db.SetupTestDB(t)
	mockSSHCommands(t, []sshMockResponse{
		{Contains: "mkdir", Stdout: ""},
		{Contains: "flock", Stdout: ""},
	})

	params := QueueJobParams{
		Host:       "test-host",
		WorkingDir: "/tmp",
		Command:    "echo",
		// QueueName omitted - should default to "default"
	}
	result, err := QueueJob(database, params, DefaultOptions())
	if err != nil {
		t.Fatalf("QueueJob failed: %v", err)
	}

	job, _ := db.GetJobByID(database, result.JobID)
	if job.QueueName != "default" {
		t.Errorf("expected queue name to be 'default', got %q", job.QueueName)
	}
}

func TestEscapeForQueueFile(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no special chars",
			input: "echo hello",
			want:  "echo hello",
		},
		{
			name:  "literal backslash-n",
			input: `python -c "import x;\nprint(x)"`,
			want:  `python -c "import x;\\nprint(x)"`,
		},
		{
			name:  "actual newline",
			input: "line1\nline2",
			want:  `line1\nline2`,
		},
		{
			name:  "actual tab",
			input: "col1\tcol2",
			want:  `col1\tcol2`,
		},
		{
			name:  "backslash not followed by n",
			input: `path\to\file`,
			want:  `path\\to\\file`,
		},
		{
			name:  "double backslash",
			input: `echo \\n`,
			want:  `echo \\\\n`,
		},
		{
			name:  "mixed escapes",
			input: "cmd\twith\ttabs\nand\nnewlines",
			want:  `cmd\twith\ttabs\nand\nnewlines`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := escapeForQueueFile(tt.input)
			if got != tt.want {
				t.Errorf("escapeForQueueFile(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestEscapeForQueueFile_RealWorldCommands(t *testing.T) {
	// These are real commands that caused issues in production
	tests := []struct {
		name    string
		command string
	}{
		{
			name:    "python with literal newline escape",
			command: `uv run python -c "import importlib;\nprint(importlib.import_module(\"h5py\").__version__)"`,
		},
		{
			name:    "multiline python command",
			command: `pixi run python -c "from collections import Counter; from safetensors import safe_open; path=\"/path/to/file\"; counts=Counter();\nwith safe_open(path) as handle:\n    for name in handle.keys():\n        tensor = handle.get_tensor(name)\nprint(counts)"`,
		},
		{
			name:    "command with tabs",
			command: "awk -F'\t' '{print $1}'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			escaped := escapeForQueueFile(tt.command)

			// Escaped command should not contain actual newlines or tabs
			if strings.Contains(escaped, "\n") {
				t.Errorf("escaped command contains actual newline: %q", escaped)
			}
			if strings.Contains(escaped, "\t") {
				t.Errorf("escaped command contains actual tab: %q", escaped)
			}

			// The escaped string should be safe to include in a tab-separated line
			// (i.e., no unescaped tabs that would be interpreted as field separators)
		})
	}
}

func TestQueueEntryFormat_SingleLine(t *testing.T) {
	// Verify that queue entries with special characters in commands
	// result in single-line entries (no actual newlines in the formatted line)
	tests := []struct {
		name    string
		command string
	}{
		{
			name:    "simple command",
			command: "echo hello",
		},
		{
			name:    "command with literal backslash-n",
			command: `python -c "print('line1');\nprint('line2')"`,
		},
		{
			name:    "command with actual newlines",
			command: "echo line1\necho line2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := QueueEntry{
				JobID:       123,
				WorkingDir:  "/tmp",
				Command:     tt.command,
				Description: "test",
			}

			// Build the queue line the same way AppendQueueEntry does
			escapedCommand := escapeForQueueFile(entry.Command)
			escapedDescription := escapeForQueueFile(entry.Description)
			jobLine := fmt.Sprintf("%d\t%s\t%s\t%s\t%s\t%s\n",
				entry.JobID, entry.WorkingDir, escapedCommand, escapedDescription, "", "")

			// Count newlines - should be exactly 1 (the trailing newline)
			newlineCount := strings.Count(jobLine, "\n")
			if newlineCount != 1 {
				t.Errorf("queue entry has %d newlines, want 1: %q", newlineCount, jobLine)
			}

			// Count tabs - should be exactly 5 (6 fields = 5 separators)
			tabCount := strings.Count(jobLine, "\t")
			if tabCount != 5 {
				t.Errorf("queue entry has %d tabs, want 5: %q", tabCount, jobLine)
			}
		})
	}
}
