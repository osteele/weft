package cmd

import (
	"testing"
)

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
