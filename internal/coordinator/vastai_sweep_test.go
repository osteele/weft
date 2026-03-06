package coordinator

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/osteele/weft/internal/vastai"
	_ "modernc.org/sqlite"
)

func TestDetectFailureReason_OOM(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int
		dmesg    string
		nvSmi    string
		want     string
	}{
		{
			name:     "exit 137 is OOM",
			exitCode: 137,
			want:     "oom",
		},
		{
			name:     "dmesg OOM killer",
			exitCode: 1,
			dmesg:    "Jan  1 00:00:00 host kernel: Out of memory: Killed process 1234",
			want:     "oom",
		},
		{
			name:     "dmesg oom-kill",
			exitCode: 1,
			dmesg:    "oom-kill:constraint=CONSTRAINT_MEMCG",
			want:     "oom",
		},
		{
			name:     "nvidia-smi CUDA OOM",
			exitCode: 1,
			nvSmi:    "CUDA_ERROR_OUT_OF_MEMORY",
			want:     "gpu_oom",
		},
		{
			name:     "nvidia-smi out of memory",
			exitCode: 1,
			nvSmi:    "RuntimeError: CUDA out of memory. Tried to allocate 2.00 GiB",
			want:     "gpu_oom",
		},
		{
			name:     "generic exit 1",
			exitCode: 1,
			want:     "error",
		},
		{
			name:     "other exit code",
			exitCode: 2,
			want:     "exit_2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()

			if tt.dmesg != "" {
				debugDir := filepath.Join(tmpDir, "debug")
				os.MkdirAll(debugDir, 0755)
				os.WriteFile(filepath.Join(debugDir, "dmesg.log"), []byte(tt.dmesg), 0644)
			}
			if tt.nvSmi != "" {
				debugDir := filepath.Join(tmpDir, "debug")
				os.MkdirAll(debugDir, 0755)
				os.WriteFile(filepath.Join(debugDir, "nvidia-smi.log"), []byte(tt.nvSmi), 0644)
			}

			got := detectFailureReason(tmpDir, tt.exitCode)
			if got != tt.want {
				t.Errorf("detectFailureReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadSummedInt64(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    *int64
	}{
		{"single value", "42\n", int64Ptr(42)},
		{"multiple values", "10\n20\n12\n", int64Ptr(42)},
		{"trailing newline", "5\n", int64Ptr(5)},
		{"no trailing newline", "7", int64Ptr(7)},
		{"empty file", "", nil},
		{"whitespace only", "  \n  \n", nil},
		{"mixed valid and invalid", "10\nbad\n20\n", int64Ptr(30)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data")
			os.WriteFile(path, []byte(tt.content), 0644)
			got := readSummedInt64(path)
			if tt.want == nil {
				if got != nil {
					t.Errorf("got %d, want nil", *got)
				}
			} else if got == nil {
				t.Errorf("got nil, want %d", *tt.want)
			} else if *got != *tt.want {
				t.Errorf("got %d, want %d", *got, *tt.want)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		got := readSummedInt64(filepath.Join(t.TempDir(), "nonexistent"))
		if got != nil {
			t.Errorf("got %d, want nil", *got)
		}
	})
}

func int64Ptr(v int64) *int64 { return &v }

func TestExtractPhaseTimings_NewFields(t *testing.T) {
	tmpDir := t.TempDir()
	jobID := int64(42)

	// Write phase files so extractPhaseTimings returns non-nil
	os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("phase_run_start_%d", jobID)), []byte("1000"), 0644)
	os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("phase_run_end_%d", jobID)), []byte("2000"), 0644)

	// Write uv sync timing (multiple invocations)
	os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("uv_sync_seconds_%d", jobID)), []byte("15\n8\n"), 0644)

	// Write post-job cache sizes
	os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("cache_uv_post_%d", jobID)), []byte("123456"), 0644)
	os.WriteFile(filepath.Join(tmpDir, fmt.Sprintf("cache_hf_post_%d", jobID)), []byte("789012"), 0644)

	timings := extractPhaseTimings(jobID, tmpDir)
	if timings == nil {
		t.Fatal("expected non-nil timings")
	}

	if timings.UVSyncSeconds == nil || *timings.UVSyncSeconds != 23 {
		t.Errorf("UVSyncSeconds = %v, want 23", timings.UVSyncSeconds)
	}
	if timings.CacheUVPostBytes == nil || *timings.CacheUVPostBytes != 123456 {
		t.Errorf("CacheUVPostBytes = %v, want 123456", timings.CacheUVPostBytes)
	}
	if timings.CacheHFPostBytes == nil || *timings.CacheHFPostBytes != 789012 {
		t.Errorf("CacheHFPostBytes = %v, want 789012", timings.CacheHFPostBytes)
	}
}

func TestCheckCloudInstanceLimitsHandlesUnavailableClient(t *testing.T) {
	database, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	// Minimal schema: ListCloudInstances runs before Available(), so the table must exist.
	if _, err := database.Exec(`CREATE TABLE cloud_instances (
		id INTEGER PRIMARY KEY,
		campaign_id INTEGER, status TEXT, provider TEXT, gpu_spec TEXT,
		gpu_class TEXT, gpu_mem_gb INTEGER, vastai_instance_id TEXT,
		max_spend_cents INTEGER, max_time_seconds INTEGER, actual_spend_cents INTEGER,
		created_at INTEGER, ready_at INTEGER, launched_at INTEGER, ended_at INTEGER,
		resolved_gpu_name TEXT, cost_per_hour_cents INTEGER, num_gpus INTEGER,
		dl_perf REAL, reliability REAL, inet_down_mbps REAL, inet_up_mbps REAL, cuda_version REAL
	)`); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	c := &Coordinator{
		db: database,
		VastaiClient: &vastai.MockClient{
			AvailableFunc: func() error {
				return fmt.Errorf("vastai CLI not installed")
			},
		},
	}

	// Should return early without panic when client.Available() fails
	c.checkCloudInstanceLimits(nil)
}
