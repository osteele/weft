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
		created_at INTEGER, launched_at INTEGER, ended_at INTEGER
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
