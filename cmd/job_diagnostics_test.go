package cmd

import (
	"os"
	"path/filepath"
	"testing"

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
