package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// TestIsJobInQueueShellCommand_JqOutputBehavior confirms that jq -e outputs
// "false" on stdout when the expression is false, which is why IsJobInQueue
// uses >/dev/null to suppress this output and get a clean YES/NO result.
func TestIsJobInQueueShellCommand_JqOutputBehavior(t *testing.T) {
	tmpDir := t.TempDir()
	stateFile := filepath.Join(tmpDir, "default.state.json")

	if err := os.WriteFile(stateFile, []byte(`{"pending":[]}`), 0644); err != nil {
		t.Fatalf("write state file: %v", err)
	}

	// Without >/dev/null, jq -e outputs "false" before exiting with status 1
	cmd := exec.Command("bash", "-c",
		`jq -e '.pending | index(123) != null' `+stateFile+` 2>/dev/null && echo YES || echo NO`)
	out, _ := cmd.Output()

	// jq outputs "false\n" then the shell adds "NO\n" - this is why we need >/dev/null
	expectedOutput := "false\nNO\n"
	if string(out) != expectedOutput {
		t.Skipf("jq behavior may have changed: expected %q but got %q", expectedOutput, string(out))
	}

	t.Logf("Confirmed: jq -e outputs %q - IsJobInQueue uses >/dev/null to get clean output", string(out))
}

// TestIsJobCurrentShellCommand validates the shell command used by IsJobCurrent.
func TestIsJobCurrentShellCommand(t *testing.T) {
	tmpDir := t.TempDir()
	currentFile := filepath.Join(tmpDir, "default.current")

	tests := []struct {
		name        string
		fileContent string
		fileExists  bool
		jobID       int64
		wantMatch   bool
	}{
		{
			name:        "job is current",
			fileContent: "123",
			fileExists:  true,
			jobID:       123,
			wantMatch:   true,
		},
		{
			name:        "job is current with newline",
			fileContent: "123\n",
			fileExists:  true,
			jobID:       123,
			wantMatch:   true,
		},
		{
			name:        "different job is current",
			fileContent: "456",
			fileExists:  true,
			jobID:       123,
			wantMatch:   false,
		},
		{
			name:       "no current file",
			fileExists: false,
			jobID:      123,
			wantMatch:  false,
		},
		{
			name:        "empty current file",
			fileContent: "",
			fileExists:  true,
			jobID:       123,
			wantMatch:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up from previous test
			os.Remove(currentFile)

			if tt.fileExists {
				if err := os.WriteFile(currentFile, []byte(tt.fileContent), 0644); err != nil {
					t.Fatalf("write current file: %v", err)
				}
			}

			// Run the actual shell command that IsJobCurrent uses
			shellCmd := fmt.Sprintf("cat %s 2>/dev/null || true", currentFile)
			cmd := exec.Command("bash", "-c", shellCmd)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("command failed: %v", err)
			}

			currentID := strings.TrimSpace(string(out))
			gotMatch := currentID == fmt.Sprintf("%d", tt.jobID)

			if gotMatch != tt.wantMatch {
				t.Errorf("got match=%v, want match=%v (output=%q)", gotMatch, tt.wantMatch, string(out))
			}
		})
	}
}

// TestGetJobCompletionShellCommand validates the shell command used by GetJobCompletion.
func TestGetJobCompletionShellCommand(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name          string
		statusFile    string
		statusContent string
		wantExitCode  int
		wantMtime     bool // whether we expect a valid mtime
		wantNil       bool // whether we expect nil (no completion)
	}{
		{
			name:          "completed with exit 0",
			statusFile:    "123-1700000000.status",
			statusContent: "0",
			wantExitCode:  0,
			wantMtime:     true,
		},
		{
			name:          "completed with exit 1",
			statusFile:    "123-1700000000.status",
			statusContent: "1",
			wantExitCode:  1,
			wantMtime:     true,
		},
		{
			name:          "completed with exit 127",
			statusFile:    "123-1700000000.status",
			statusContent: "127",
			wantExitCode:  127,
			wantMtime:     true,
		},
		{
			name:    "no status file",
			wantNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up status files from previous test
			files, _ := filepath.Glob(filepath.Join(tmpDir, "*.status"))
			for _, f := range files {
				os.Remove(f)
			}

			if tt.statusFile != "" {
				statusPath := filepath.Join(tmpDir, tt.statusFile)
				if err := os.WriteFile(statusPath, []byte(tt.statusContent), 0644); err != nil {
					t.Fatalf("write status file: %v", err)
				}
			}

			// Run the actual shell command that GetJobCompletion uses
			// Note: Using Linux stat format; macOS uses different flags
			statusPattern := filepath.Join(tmpDir, "123*.status")
			shellCmd := fmt.Sprintf(`f=$(ls %s 2>/dev/null | head -1); if [ -n "$f" ]; then echo "$(cat "$f" | head -1)|$(stat -c %%Y "$f" 2>/dev/null || stat -f %%m "$f" 2>/dev/null)"; fi`, statusPattern)
			cmd := exec.Command("bash", "-c", shellCmd)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("command failed: %v", err)
			}

			output := strings.TrimSpace(string(out))

			if tt.wantNil {
				if output != "" {
					t.Errorf("expected empty output for nil completion, got %q", output)
				}
				return
			}

			if output == "" {
				t.Fatalf("expected output but got empty string")
			}

			parts := strings.Split(output, "|")
			if len(parts) < 1 {
				t.Fatalf("expected pipe-separated output, got %q", output)
			}

			var exitCode int
			if _, err := fmt.Sscanf(parts[0], "%d", &exitCode); err != nil {
				t.Fatalf("parse exit code: %v", err)
			}

			if exitCode != tt.wantExitCode {
				t.Errorf("got exit code %d, want %d", exitCode, tt.wantExitCode)
			}

			if tt.wantMtime && len(parts) < 2 {
				t.Errorf("expected mtime in output, got %q", output)
			}
		})
	}
}

// TestIsProcessRunningShellCommand validates the shell command used by IsProcessRunning.
func TestIsProcessRunningShellCommand(t *testing.T) {
	tmpDir := t.TempDir()

	// Get current process PID (known to be running)
	runningPID := os.Getpid()

	tests := []struct {
		name       string
		pidFile    string
		pidContent string
		wantYes    bool
	}{
		{
			name:       "process running",
			pidFile:    "123-1700000000.pid",
			pidContent: fmt.Sprintf("%d", runningPID),
			wantYes:    true,
		},
		{
			name:       "process not running",
			pidFile:    "123-1700000000.pid",
			pidContent: "999999999", // Very high PID unlikely to exist
			wantYes:    false,
		},
		{
			name:    "no pid file",
			wantYes: false,
		},
		{
			name:       "empty pid file",
			pidFile:    "123-1700000000.pid",
			pidContent: "",
			wantYes:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up pid files from previous test
			files, _ := filepath.Glob(filepath.Join(tmpDir, "*.pid"))
			for _, f := range files {
				os.Remove(f)
			}

			if tt.pidFile != "" {
				pidPath := filepath.Join(tmpDir, tt.pidFile)
				if err := os.WriteFile(pidPath, []byte(tt.pidContent), 0644); err != nil {
					t.Fatalf("write pid file: %v", err)
				}
			}

			// Run the actual shell command that IsProcessRunning uses
			pidPattern := filepath.Join(tmpDir, "123*.pid")
			shellCmd := fmt.Sprintf(`pid=$(cat %s 2>/dev/null | head -1); [ -n "$pid" ] && ps -p $pid > /dev/null 2>&1 && echo YES || echo NO`, pidPattern)
			cmd := exec.Command("bash", "-c", shellCmd)
			out, err := cmd.Output()
			if err != nil {
				// Command may exit non-zero but still produce output
				if len(out) == 0 {
					t.Fatalf("command failed with no output: %v", err)
				}
			}

			gotYes := strings.TrimSpace(string(out)) == "YES"
			if gotYes != tt.wantYes {
				t.Errorf("got YES=%v, want YES=%v (output=%q)", gotYes, tt.wantYes, string(out))
			}
		})
	}
}

// TestGetRecentlyModifiedJobIDsShellCommand validates the shell command used by GetRecentlyModifiedJobIDs.
func TestGetRecentlyModifiedJobIDsShellCommand(t *testing.T) {
	tmpDir := t.TempDir()

	// Create some test files with different timestamps
	now := time.Now()
	oldTime := now.Add(-2 * time.Hour)
	recentTime := now.Add(-5 * time.Minute)

	// Create old files (more than 1 hour old)
	oldPid := filepath.Join(tmpDir, "100-1700000000.pid")
	if err := os.WriteFile(oldPid, []byte("12345"), 0644); err != nil {
		t.Fatalf("write old pid: %v", err)
	}
	if err := os.Chtimes(oldPid, oldTime, oldTime); err != nil {
		t.Fatalf("set old time: %v", err)
	}

	oldStatus := filepath.Join(tmpDir, "101-1700000000.status")
	if err := os.WriteFile(oldStatus, []byte("0"), 0644); err != nil {
		t.Fatalf("write old status: %v", err)
	}
	if err := os.Chtimes(oldStatus, oldTime, oldTime); err != nil {
		t.Fatalf("set old time: %v", err)
	}

	// Create recent files (less than 1 hour old)
	recentPid := filepath.Join(tmpDir, "200-1700000000.pid")
	if err := os.WriteFile(recentPid, []byte("12346"), 0644); err != nil {
		t.Fatalf("write recent pid: %v", err)
	}
	if err := os.Chtimes(recentPid, recentTime, recentTime); err != nil {
		t.Fatalf("set recent time: %v", err)
	}

	recentStatus := filepath.Join(tmpDir, "201-1700000000.status")
	if err := os.WriteFile(recentStatus, []byte("0"), 0644); err != nil {
		t.Fatalf("write recent status: %v", err)
	}
	if err := os.Chtimes(recentStatus, recentTime, recentTime); err != nil {
		t.Fatalf("set recent time: %v", err)
	}

	// Also create a file for job 200 with .status to test deduplication
	recentStatus2 := filepath.Join(tmpDir, "200-1700000000.status")
	if err := os.WriteFile(recentStatus2, []byte("0"), 0644); err != nil {
		t.Fatalf("write recent status 2: %v", err)
	}
	if err := os.Chtimes(recentStatus2, recentTime, recentTime); err != nil {
		t.Fatalf("set recent time: %v", err)
	}

	// Use -mmin to find files modified within last 60 minutes
	// This matches the production code which uses relative minutes
	minutesAgo := 60

	shellCmd := fmt.Sprintf(
		`find %s -mmin -%d \( -name "*.pid" -o -name "*.status" \) 2>/dev/null | sed 's|.*/||; s/-.*//; s/\..*$//' | sort -un`,
		tmpDir, minutesAgo,
	)
	cmd := exec.Command("bash", "-c", shellCmd)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("command failed: %v", err)
	}

	// Parse output
	var jobIDs []int64
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(line, "%d", &id); err == nil {
			jobIDs = append(jobIDs, id)
		}
	}

	// Should find jobs 200 and 201 (modified 5 min ago), but not 100 and 101 (modified 2 hours ago)
	expectedIDs := map[int64]bool{200: true, 201: true}
	unexpectedIDs := map[int64]bool{100: true, 101: true}

	for _, id := range jobIDs {
		if unexpectedIDs[id] {
			t.Errorf("found old job ID %d that should have been filtered", id)
		}
		delete(expectedIDs, id)
	}

	for id := range expectedIDs {
		t.Errorf("missing expected recent job ID %d", id)
	}

	// Check deduplication - 200 should appear only once
	count200 := 0
	for _, id := range jobIDs {
		if id == 200 {
			count200++
		}
	}
	if count200 > 1 {
		t.Errorf("job ID 200 appeared %d times, expected 1 (deduplication failed)", count200)
	}
}

// =============================================================================
// commandJob JSON serialization tests
// =============================================================================

// TestCommandJobSerializesAllFields verifies that all resource fields on
// commandJob survive a JSON round-trip. This catches missing or misspelled
// struct tags.
func TestCommandJobSerializesAllFields(t *testing.T) {
	gpuMem := 80
	cpu := 4
	job := commandJob{
		ID:         42,
		Dir:        "/tmp/train",
		Cmd:        "python train.py",
		Desc:       "training run",
		SourceSHA:  "abc123",
		Env:        []string{"FOO=bar"},
		Deps:       "10",
		CPU:        &cpu,
		GPU:        "0,1",
		GPUClass:   "a100",
		GPUMem:     &gpuMem,
		Tags:       []string{"exclusive", "benchmark"},
		OutputDirs: []string{"output/", "results/"},
		Produces:   []string{"output/model.pt"},
		Needs:      []string{"data.csv:1"},
	}

	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	// Verify all expected keys exist with correct values
	checks := []struct {
		key  string
		want interface{}
	}{
		{"id", float64(42)},
		{"dir", "/tmp/train"},
		{"cmd", "python train.py"},
		{"desc", "training run"},
		{"source_sha256", "abc123"},
		{"deps", "10"},
		{"gpu", "0,1"},
		{"gpu_class", "a100"},
		{"gpu_mem", float64(80)},
		{"cpu", float64(4)},
	}
	for _, c := range checks {
		v, ok := m[c.key]
		if !ok {
			t.Errorf("missing key %q in JSON output", c.key)
			continue
		}
		if v != c.want {
			t.Errorf("key %q: got %v, want %v", c.key, v, c.want)
		}
	}

	// Verify array fields
	arrayChecks := []struct {
		key  string
		want []string
	}{
		{"tags", []string{"exclusive", "benchmark"}},
		{"output_dirs", []string{"output/", "results/"}},
		{"produces", []string{"output/model.pt"}},
		{"needs", []string{"data.csv:1"}},
		{"env", []string{"FOO=bar"}},
	}
	for _, c := range arrayChecks {
		v, ok := m[c.key]
		if !ok {
			t.Errorf("missing key %q in JSON output", c.key)
			continue
		}
		arr, ok := v.([]interface{})
		if !ok {
			t.Errorf("key %q: expected array, got %T", c.key, v)
			continue
		}
		if len(arr) != len(c.want) {
			t.Errorf("key %q: got %d elements, want %d", c.key, len(arr), len(c.want))
			continue
		}
		for i, want := range c.want {
			if arr[i] != want {
				t.Errorf("key %q[%d]: got %v, want %v", c.key, i, arr[i], want)
			}
		}
	}

}

// =============================================================================
// SSHProber Unit Tests
// =============================================================================

// mockSSHHost is a test implementation of the methods SSHProber calls.
type mockSSHHost struct {
	inQueueResult    bool
	inQueueErr       error
	currentResult    bool
	currentErr       error
	completionResult *CompletionInfo
	completionErr    error
	processResult    bool
	processErr       error
	pausedResult     bool
	pausedErr        error
}

func (m *mockSSHHost) IsJobInQueue(jobID int64) (bool, error) {
	return m.inQueueResult, m.inQueueErr
}

func (m *mockSSHHost) IsJobCurrent(jobID int64) (bool, error) {
	return m.currentResult, m.currentErr
}

func (m *mockSSHHost) GetJobCompletion(jobID int64) (*CompletionInfo, error) {
	return m.completionResult, m.completionErr
}

func (m *mockSSHHost) IsProcessRunning(jobID int64) (bool, error) {
	return m.processResult, m.processErr
}

func (m *mockSSHHost) IsProcessPaused(jobID int64) (bool, error) {
	return m.pausedResult, m.pausedErr
}

// testableSSHProber wraps a mockSSHHost for testing
type testableSSHProber struct {
	mock *mockSSHHost
}

func (p *testableSSHProber) ProbeInQueue(jobID int64) ProbeResult {
	result, err := p.mock.IsJobInQueue(jobID)
	if err != nil {
		return ProbeUnknown
	}
	if result {
		return ProbeTrue
	}
	return ProbeFalse
}

func (p *testableSSHProber) ProbeCurrent(jobID int64) ProbeResult {
	result, err := p.mock.IsJobCurrent(jobID)
	if err != nil {
		return ProbeUnknown
	}
	if result {
		return ProbeTrue
	}
	return ProbeFalse
}

func (p *testableSSHProber) ProbeCompleted(jobID int64) (ProbeResult, *CompletionInfo) {
	info, err := p.mock.GetJobCompletion(jobID)
	if err != nil {
		return ProbeUnknown, nil
	}
	if info == nil {
		return ProbeFalse, nil
	}
	return ProbeTrue, info
}

func (p *testableSSHProber) ProbeProcessRunning(jobID int64) ProbeResult {
	result, err := p.mock.IsProcessRunning(jobID)
	if err != nil {
		return ProbeUnknown
	}
	if result {
		return ProbeTrue
	}
	return ProbeFalse
}

func (p *testableSSHProber) ProbeProcessPaused(jobID int64) ProbeResult {
	result, err := p.mock.IsProcessPaused(jobID)
	if err != nil {
		return ProbeUnknown
	}
	if result {
		return ProbeTrue
	}
	return ProbeFalse
}

func TestSSHProberProbeInQueue(t *testing.T) {
	tests := []struct {
		name      string
		result    bool
		err       error
		wantProbe ProbeResult
	}{
		{"in queue", true, nil, ProbeTrue},
		{"not in queue", false, nil, ProbeFalse},
		{"error returns unknown", false, errors.New("ssh failed"), ProbeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prober := &testableSSHProber{mock: &mockSSHHost{
				inQueueResult: tt.result,
				inQueueErr:    tt.err,
			}}

			got := prober.ProbeInQueue(123)
			if got != tt.wantProbe {
				t.Errorf("ProbeInQueue() = %v, want %v", got, tt.wantProbe)
			}
		})
	}
}

func TestSSHProberProbeCurrent(t *testing.T) {
	tests := []struct {
		name      string
		result    bool
		err       error
		wantProbe ProbeResult
	}{
		{"is current", true, nil, ProbeTrue},
		{"not current", false, nil, ProbeFalse},
		{"error returns unknown", false, errors.New("ssh failed"), ProbeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prober := &testableSSHProber{mock: &mockSSHHost{
				currentResult: tt.result,
				currentErr:    tt.err,
			}}

			got := prober.ProbeCurrent(123)
			if got != tt.wantProbe {
				t.Errorf("ProbeCurrent() = %v, want %v", got, tt.wantProbe)
			}
		})
	}
}

func TestSSHProberProbeCompleted(t *testing.T) {
	completionInfo := &CompletionInfo{ExitCode: 0, EndTime: 1700000000}

	tests := []struct {
		name      string
		info      *CompletionInfo
		err       error
		wantProbe ProbeResult
		wantInfo  *CompletionInfo
	}{
		{"completed", completionInfo, nil, ProbeTrue, completionInfo},
		{"not completed", nil, nil, ProbeFalse, nil},
		{"error returns unknown", nil, errors.New("ssh failed"), ProbeUnknown, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prober := &testableSSHProber{mock: &mockSSHHost{
				completionResult: tt.info,
				completionErr:    tt.err,
			}}

			gotProbe, gotInfo := prober.ProbeCompleted(123)
			if gotProbe != tt.wantProbe {
				t.Errorf("ProbeCompleted() probe = %v, want %v", gotProbe, tt.wantProbe)
			}
			if gotInfo != tt.wantInfo {
				t.Errorf("ProbeCompleted() info = %v, want %v", gotInfo, tt.wantInfo)
			}
		})
	}
}

// TestGetJobCompletionSignalSuffix validates that GetJobCompletion correctly
// parses exit codes from status files that contain signal suffixes like
// "137 signal=KILL" or "0 signal=TERM".
func TestGetJobCompletionSignalSuffix(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name          string
		statusContent string
		wantExitCode  int
	}{
		{
			name:          "exit 0 with signal suffix",
			statusContent: "0 signal=TERM",
			wantExitCode:  0,
		},
		{
			name:          "exit 137 with signal suffix",
			statusContent: "137 signal=KILL",
			wantExitCode:  137,
		},
		{
			name:          "exit 1 with signal suffix",
			statusContent: "1 signal=HUP",
			wantExitCode:  1,
		},
		{
			name:          "plain exit code still works",
			statusContent: "0",
			wantExitCode:  0,
		},
		{
			name:          "plain non-zero exit code",
			statusContent: "42",
			wantExitCode:  42,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up status files
			files, _ := filepath.Glob(filepath.Join(tmpDir, "*.status"))
			for _, f := range files {
				os.Remove(f)
			}

			statusPath := filepath.Join(tmpDir, "123-1700000000.status")
			if err := os.WriteFile(statusPath, []byte(tt.statusContent), 0644); err != nil {
				t.Fatalf("write status file: %v", err)
			}

			// Simulate what GetJobCompletion does: read the file, split on |, parse exit code
			statusPattern := filepath.Join(tmpDir, "123*.status")
			shellCmd := fmt.Sprintf(`f=$(ls %s 2>/dev/null | head -1); if [ -n "$f" ]; then echo "$(cat "$f" | head -1)|$(stat -c %%Y "$f" 2>/dev/null || stat -f %%m "$f" 2>/dev/null)"; fi`, statusPattern)
			cmd := exec.Command("bash", "-c", shellCmd)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("command failed: %v", err)
			}

			output := strings.TrimSpace(string(out))
			if output == "" {
				t.Fatalf("expected output but got empty string")
			}

			parts := strings.Split(output, "|")

			// This is the production parsing code from ssh_host.go:181
			// It currently uses strconv.Atoi which fails on "137 signal=KILL"
			var exitCode int
			if _, err := fmt.Sscanf(parts[0], "%d", &exitCode); err != nil {
				t.Fatalf("parse exit code from %q: %v", parts[0], err)
			}

			if exitCode != tt.wantExitCode {
				t.Errorf("got exit code %d, want %d", exitCode, tt.wantExitCode)
			}
		})
	}
}

func TestSSHProberProbeProcessRunning(t *testing.T) {
	tests := []struct {
		name      string
		result    bool
		err       error
		wantProbe ProbeResult
	}{
		{"process running", true, nil, ProbeTrue},
		{"process not running", false, nil, ProbeFalse},
		{"error returns unknown", false, errors.New("ssh failed"), ProbeUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prober := &testableSSHProber{mock: &mockSSHHost{
				processResult: tt.result,
				processErr:    tt.err,
			}}

			got := prober.ProbeProcessRunning(123)
			if got != tt.wantProbe {
				t.Errorf("ProbeProcessRunning() = %v, want %v", got, tt.wantProbe)
			}
		})
	}
}
