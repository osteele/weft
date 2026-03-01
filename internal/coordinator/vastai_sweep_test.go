package coordinator

import (
	"os"
	"path/filepath"
	"testing"
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
