package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestShouldFireHook(t *testing.T) {
	tests := []struct {
		name      string
		oldStatus string
		newStatus string
		want      bool
	}{
		{"running to completed", db.StatusRunning, db.StatusCompleted, true},
		{"running to failed", db.StatusRunning, db.StatusFailed, true},
		{"running to killed", db.StatusRunning, db.StatusKilled, true},
		{"running to dead", db.StatusRunning, db.StatusDead, true},
		{"queued to canceled", db.StatusQueued, db.StatusCanceled, false},
		{"queued to draft", db.StatusQueued, db.StatusDraft, false},
		{"completed to completed", db.StatusCompleted, db.StatusCompleted, false},
		{"dead to failed", db.StatusDead, db.StatusFailed, false},
		{"running to running", db.StatusRunning, db.StatusRunning, false},
		{"running to paused", db.StatusRunning, db.StatusPaused, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldFireHook(tt.oldStatus, tt.newStatus); got != tt.want {
				t.Errorf("ShouldFireHook(%q, %q) = %v, want %v", tt.oldStatus, tt.newStatus, got, tt.want)
			}
		})
	}
}

func TestRunOnJobComplete_ScriptNotPresent(t *testing.T) {
	// Ensure no hook script exists at the expected path
	path := hookPath()
	if path == "" {
		t.Skip("could not determine hook path")
	}
	// If the hook happens to exist on the test machine, skip this test
	if _, err := os.Stat(path); err == nil {
		t.Skip("hook script exists on this machine, skipping no-op test")
	}

	job := &db.Job{
		ID:     999,
		Host:   "testhost",
		Status: db.StatusCompleted,
	}
	// Should not panic or block
	RunOnJobComplete(job)
}

func TestRunOnJobComplete_FiresWithEnvVars(t *testing.T) {
	// Create a temporary hook script that writes env vars to a file
	tmpDir := t.TempDir()
	hookScript := filepath.Join(tmpDir, "on-job-complete")
	outputFile := filepath.Join(tmpDir, "hook-output.txt")

	script := "#!/bin/bash\n" +
		"echo \"JOB_ID=$JOB_ID\" >> " + outputFile + "\n" +
		"echo \"JOB_HOST=$JOB_HOST\" >> " + outputFile + "\n" +
		"echo \"JOB_STATUS=$JOB_STATUS\" >> " + outputFile + "\n" +
		"echo \"JOB_DESCRIPTION=$JOB_DESCRIPTION\" >> " + outputFile + "\n" +
		"echo \"JOB_DIR=$JOB_DIR\" >> " + outputFile + "\n" +
		"echo \"JOB_PROJECT=$JOB_PROJECT\" >> " + outputFile + "\n"

	if err := os.WriteFile(hookScript, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}

	job := &db.Job{
		ID:          42,
		Host:        "cool30",
		Status:      db.StatusCompleted,
		Description: "test job",
		WorkingDir:  "/home/user/project",
		Project:     "my-project",
	}

	// Call runHook directly (synchronous) to test env vars
	runHook(hookScript, job)

	content, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("failed to read hook output: %v", err)
	}

	output := string(content)
	expected := map[string]string{
		"JOB_ID":          "42",
		"JOB_HOST":        "cool30",
		"JOB_STATUS":      "completed",
		"JOB_DESCRIPTION": "test job",
		"JOB_DIR":         "/home/user/project",
		"JOB_PROJECT":     "my-project",
	}

	for key, val := range expected {
		needle := key + "=" + val
		if !strings.Contains(output, needle) {
			t.Errorf("hook output missing %s, got:\n%s", needle, output)
		}
	}
}
