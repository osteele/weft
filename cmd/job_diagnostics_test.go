package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/remediation"
)

func mustMarshalDiagnosis(t *testing.T, d *remediation.ErrorDiagnosis) string {
	t.Helper()
	s, err := remediation.MarshalDiagnosis(d)
	if err != nil {
		t.Fatalf("marshal diagnosis: %v", err)
	}
	return s
}

// TestResolveJobDiagnosis covers the stored→log-cache fallback matrix.
// The fallback is what lets `weft job info` / `weft job status` surface
// a diagnosis for jobs that never invoked the remediation pipeline (the
// EXP-179 wj2041 regression — see specs/job-lifecycle.allium).
func TestResolveJobDiagnosis(t *testing.T) {
	storedOOM := mustMarshalDiagnosis(t, &remediation.ErrorDiagnosis{
		Pattern: "module_not_found", Category: "environment", Message: "missing module: torch",
	})
	zero := 0

	cases := []struct {
		name          string
		job           *db.Job
		logContent    string
		writeEmptyLog bool
		wantPattern   string // "" = expect nil result
	}{
		{
			name: "nil job returns nil",
			job:  nil,
		},
		{
			name:        "stored wins over log content",
			job:         &db.Job{ID: 42, ErrorDiagnosis: storedOOM},
			logContent:  "torch.cuda.OutOfMemoryError: CUDA out of memory\n",
			wantPattern: "module_not_found",
		},
		{
			name:        "fallback to log when stored is empty",
			job:         &db.Job{ID: 2041},
			logContent:  "RuntimeError: CUDA out of memory. Tried to allocate 4.70 GiB.\n",
			wantPattern: "gpu_oom",
		},
		{
			name:        "corrupt stored falls through to log",
			job:         &db.Job{ID: 7, ErrorDiagnosis: "{not valid json"},
			logContent:  "torch.cuda.OutOfMemoryError\n",
			wantPattern: "gpu_oom",
		},
		{
			name:       "clean completed job ignores stale log-cache diagnosis",
			job:        &db.Job{ID: 4615, Status: db.StatusCompleted, ExitCode: &zero},
			logContent: "Execution timed out after 4h\nRuntimeError: CUDA out of memory\n",
		},
		{
			name: "no stored and no log returns nil",
			job:  &db.Job{ID: 9999},
		},
		{
			name:          "empty log file returns nil",
			job:           &db.Job{ID: 5},
			writeEmptyLog: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempHome := t.TempDir()
			t.Setenv("HOME", tempHome)
			switch {
			case tc.writeEmptyLog:
				logPath := logcache.CachePath(tc.job.ID)
				if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
					t.Fatalf("mkdir cache: %v", err)
				}
				if err := os.WriteFile(logPath, nil, 0o644); err != nil {
					t.Fatalf("write empty log: %v", err)
				}
			case tc.logContent != "":
				if err := logcache.Write(tc.job.ID, tc.logContent); err != nil {
					t.Fatalf("write log cache: %v", err)
				}
			}

			got := ResolveJobDiagnosis(tc.job)
			switch {
			case tc.wantPattern == "" && got != nil:
				t.Errorf("got %+v, want nil", got)
			case tc.wantPattern != "" && got == nil:
				t.Errorf("got nil, want pattern %q", tc.wantPattern)
			case tc.wantPattern != "" && got.Pattern != tc.wantPattern:
				t.Errorf("Pattern = %q, want %q", got.Pattern, tc.wantPattern)
			}
		})
	}
}

// TestHasFailureSignal gates the local-diagnostics block: in-progress and
// clean-completed jobs must skip the log-cache scan to keep the hot path
// cheap.
func TestHasFailureSignal(t *testing.T) {
	nonZero := 1
	zero := 0
	cases := []struct {
		name string
		job  *db.Job
		want bool
	}{
		{"nil", nil, false},
		{"queued", &db.Job{Status: db.StatusQueued}, false},
		{"running", &db.Job{Status: db.StatusRunning}, false},
		{"clean completed", &db.Job{Status: db.StatusCompleted, ExitCode: &zero}, false},
		{"failed status", &db.Job{Status: db.StatusFailed}, true},
		{"dead status", &db.Job{Status: db.StatusDead}, true},
		{"killed status", &db.Job{Status: db.StatusKilled}, true},
		{"non-zero exit", &db.Job{Status: db.StatusCompleted, ExitCode: &nonZero}, true},
		{"failure metadata on completed status", &db.Job{Status: db.StatusCompleted, FailureReason: "gpu_oom"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasFailureSignal(tc.job); got != tc.want {
				t.Errorf("hasFailureSignal = %v, want %v", got, tc.want)
			}
		})
	}
}

// Regression (wb18): weft info must show the constraint that actually
// rejects hosts (the driver/CUDA floor) with its provenance, not just the
// usually-inert arch cap.
func TestDriverFloorLine_TorchPinProvenance(t *testing.T) {
	dir := t.TempDir()
	dataloc.WriteTestTorchPin(t, dir, "2.9.1", "cu128")
	mem := 8
	job := &db.Job{WorkingDir: dir, Command: "uv run train.py", GPUMemGB: &mem}

	line := driverFloorLine(job)
	if !strings.Contains(line, ">=570") {
		t.Errorf("driverFloorLine = %q, want driver >=570 (operational floor)", line)
	}
	if !strings.Contains(line, "CUDA >=12.8") || !strings.Contains(line, "torch pin") {
		t.Errorf("driverFloorLine = %q, want cloud CUDA floor with torch provenance", line)
	}
}

func TestDriverFloorLine_UsesCloudExactTorchCUDAFloor(t *testing.T) {
	dir := t.TempDir()
	dataloc.WriteTestTorchPin(t, dir, "2.6.0", "cu124")
	mem := 20
	job := &db.Job{WorkingDir: dir, Command: "uv run train.py", GPUMemGB: &mem}

	line := driverFloorLine(job)
	if !strings.Contains(line, ">=550") {
		t.Errorf("driverFloorLine = %q, want cloud driver >=550", line)
	}
	if !strings.Contains(line, "CUDA >=12.4") {
		t.Errorf("driverFloorLine = %q, want exact cloud CUDA floor 12.4", line)
	}
}

func TestDriverFloorLine_NoTorchPin(t *testing.T) {
	mem := 8
	job := &db.Job{WorkingDir: t.TempDir(), Command: "python x.py", GPUMemGB: &mem}
	if line := driverFloorLine(job); line != "" {
		t.Errorf("driverFloorLine = %q, want empty for project without torch pin", line)
	}
}
