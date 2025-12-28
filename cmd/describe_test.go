package cmd

import (
	"testing"
)

func TestExtractGPUFromEnvVars(t *testing.T) {
	tests := []struct {
		name    string
		envVars []string
		want    string
	}{
		{
			name:    "no env vars",
			envVars: nil,
			want:    "",
		},
		{
			name:    "empty env vars",
			envVars: []string{},
			want:    "",
		},
		{
			name:    "no CUDA var",
			envVars: []string{"FOO=bar", "BATCH_SIZE=32"},
			want:    "",
		},
		{
			name:    "CUDA var only",
			envVars: []string{"CUDA_VISIBLE_DEVICES=0"},
			want:    "0",
		},
		{
			name:    "CUDA var with others",
			envVars: []string{"FOO=bar", "CUDA_VISIBLE_DEVICES=2", "BATCH_SIZE=32"},
			want:    "2",
		},
		{
			name:    "multiple GPUs",
			envVars: []string{"CUDA_VISIBLE_DEVICES=0,1,2"},
			want:    "0,1,2",
		},
		{
			name:    "CUDA var first",
			envVars: []string{"CUDA_VISIBLE_DEVICES=5", "OTHER=value"},
			want:    "5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractGPUFromEnvVars(tt.envVars)
			if got != tt.want {
				t.Errorf("extractGPUFromEnvVars(%v) = %q, want %q", tt.envVars, got, tt.want)
			}
		})
	}
}

func TestUpdateCudaVisibleDevices(t *testing.T) {
	tests := []struct {
		name     string
		cmd      string
		gpuValue string
		want     string
	}{
		{
			name:     "add GPU to simple command",
			cmd:      "python train.py",
			gpuValue: "0",
			want:     "CUDA_VISIBLE_DEVICES=0 python train.py",
		},
		{
			name:     "add multiple GPUs",
			cmd:      "python train.py",
			gpuValue: "0,1,2",
			want:     "CUDA_VISIBLE_DEVICES=0,1,2 python train.py",
		},
		{
			name:     "replace existing GPU at start",
			cmd:      "CUDA_VISIBLE_DEVICES=0 python train.py",
			gpuValue: "1",
			want:     "CUDA_VISIBLE_DEVICES=1 python train.py",
		},
		{
			name:     "replace existing GPU with export",
			cmd:      "export CUDA_VISIBLE_DEVICES=0 && python train.py",
			gpuValue: "2",
			want:     "CUDA_VISIBLE_DEVICES=2 python train.py",
		},
		{
			name:     "replace multiple GPUs",
			cmd:      "CUDA_VISIBLE_DEVICES=0,1 python train.py",
			gpuValue: "2,3",
			want:     "CUDA_VISIBLE_DEVICES=2,3 python train.py",
		},
		{
			name:     "command with cd prefix",
			cmd:      "cd /foo && python train.py",
			gpuValue: "0",
			want:     "CUDA_VISIBLE_DEVICES=0 cd /foo && python train.py",
		},
		{
			name:     "replace GPU with cd prefix",
			cmd:      "CUDA_VISIBLE_DEVICES=1 cd /foo && python train.py",
			gpuValue: "0",
			want:     "CUDA_VISIBLE_DEVICES=0 cd /foo && python train.py",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := updateCudaVisibleDevices(tt.cmd, tt.gpuValue)
			if got != tt.want {
				t.Errorf("updateCudaVisibleDevices(%q, %q) = %q, want %q", tt.cmd, tt.gpuValue, got, tt.want)
			}
		})
	}
}
