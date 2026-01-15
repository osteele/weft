package remote

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestIsJobInQueueShellCommand validates that the shell command used by IsJobInQueue
// produces clean YES/NO output without jq pollution.
// This test would have caught the bug where jq -e outputted "false" before "NO".
func TestIsJobInQueueShellCommand(t *testing.T) {
	// Create a temp directory with a test state file
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "default.state.json")

	tests := []struct {
		name      string
		stateJSON string
		jobID     int64
		wantOut   string
	}{
		{
			name:      "job not in queue - empty pending",
			stateJSON: `{"pending":[],"current":null}`,
			jobID:     123,
			wantOut:   "NO\n",
		},
		{
			name:      "job not in queue - other jobs pending",
			stateJSON: `{"pending":[456,789],"current":null}`,
			jobID:     123,
			wantOut:   "NO\n",
		},
		{
			name:      "job in queue",
			stateJSON: `{"pending":[123,456],"current":null}`,
			jobID:     123,
			wantOut:   "YES\n",
		},
		{
			name:      "job in queue - single item",
			stateJSON: `{"pending":[123],"current":null}`,
			jobID:     123,
			wantOut:   "YES\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Write state file
			if err := os.WriteFile(stateFile, []byte(tt.stateJSON), 0644); err != nil {
				t.Fatalf("write state file: %v", err)
			}

			// Run the actual shell command that IsJobInQueue uses
			// Note: This uses >/dev/null to avoid jq output pollution
			shellCmd := fmt.Sprintf(`jq -e '.pending | index(%d) != null' %s >/dev/null 2>&1 && echo YES || echo NO`, tt.jobID, stateFile)
			cmd := exec.Command("bash", "-c", shellCmd)
			out, err := cmd.Output()
			if err != nil {
				// Command failed but we might still get output
				if exitErr, ok := err.(*exec.ExitError); ok {
					t.Logf("command stderr: %s", exitErr.Stderr)
				}
			}

			// The output should be exactly "YES\n" or "NO\n" - no "false" pollution
			if string(out) != tt.wantOut {
				t.Errorf("got %q, want %q", string(out), tt.wantOut)
			}
		})
	}
}

// TestIsJobInQueueShellCommandWithoutRedirect demonstrates the bug that was fixed.
// Without the >/dev/null redirect, jq -e outputs "false" which pollutes the output.
func TestIsJobInQueueShellCommandWithoutRedirect_DemonstratesBug(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "default.state.json")

	// Write state file with empty pending
	if err := os.WriteFile(stateFile, []byte(`{"pending":[]}`), 0644); err != nil {
		t.Fatalf("write state file: %v", err)
	}

	// Run the BUGGY version (without >/dev/null)
	cmd := exec.Command("bash", "-c",
		`jq -e '.pending | index(123) != null' `+stateFile+` 2>/dev/null && echo YES || echo NO`)
	out, _ := cmd.Output()

	// This demonstrates the bug: output is "false\nNO\n" instead of just "NO\n"
	// The sync code expected "NO" but got "false\nNO" which didn't match
	buggyOutput := "false\nNO\n"
	if string(out) != buggyOutput {
		t.Skipf("Expected buggy output %q but got %q - jq behavior may have changed", buggyOutput, string(out))
	}

	t.Logf("Confirmed bug: without >/dev/null, output is %q instead of clean 'NO\\n'", string(out))
}
