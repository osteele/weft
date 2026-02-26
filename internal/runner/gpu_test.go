package runner

import (
	"testing"

	"github.com/osteele/weft/internal/ops"
)

func TestDevicesByClass(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
			{Index: "1", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
			{Index: "2", Name: "NVIDIA GeForce RTX 2080 Ti", TotalMemGB: 11},
			{Index: "3", Name: "NVIDIA GeForce RTX 3090", TotalMemGB: 24},
		},
	}

	tests := []struct {
		className string
		wantCount int
	}{
		{"A100", 2},
		{"a100", 2},
		{"2080", 1},
		{"3090", 1},
		{"RTX", 2}, // Matches both 2080 and 3090
		{"H100", 0},
	}

	for _, tt := range tests {
		got := inv.DevicesByClass(tt.className)
		if len(got) != tt.wantCount {
			t.Errorf("DevicesByClass(%q) = %v (len %d), want len %d", tt.className, got, len(got), tt.wantCount)
		}
	}
}

func TestPickBestGPUForClass(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
			{Index: "1", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
			{Index: "2", Name: "NVIDIA GeForce RTX 2080 Ti", TotalMemGB: 11},
		},
	}

	state := NewState()

	// First pick should succeed
	device, ok := inv.PickBestGPUForClass(state, "A100", 20)
	if !ok {
		t.Fatal("expected a device")
	}
	if device != "0" && device != "1" {
		t.Errorf("expected 0 or 1, got %s", device)
	}

	// Mark device 0 as in use
	state.AddRunning("100", RunningJobState{GPUDevices: []string{"0"}, GPUMemGB: 20})

	// Second pick should get device 1
	device, ok = inv.PickBestGPUForClass(state, "A100", 20)
	if !ok {
		t.Fatal("expected a device")
	}
	if device != "1" {
		t.Errorf("expected 1, got %s", device)
	}

	// Mark device 1 as in use too
	state.AddRunning("101", RunningJobState{GPUDevices: []string{"1"}, GPUMemGB: 20})

	// No more A100s available
	_, ok = inv.PickBestGPUForClass(state, "A100", 20)
	if ok {
		t.Error("expected no device available")
	}
}

func TestPickBestGPUForClass_MemoryCheck(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
		},
	}
	state := NewState()

	// Request more memory than available
	_, ok := inv.PickBestGPUForClass(state, "A100", 90)
	if ok {
		t.Error("expected no device (memory too high)")
	}

	// Request within memory
	device, ok := inv.PickBestGPUForClass(state, "A100", 40)
	if !ok {
		t.Fatal("expected device")
	}
	if device != "0" {
		t.Errorf("expected 0, got %s", device)
	}
}

func TestCanStartGPUJob_CPUOnly(t *testing.T) {
	inv := &GPUInventory{}
	state := NewState()

	job := &RunnerJob{
		Data: &ops.CommandJob{ID: 1, Cmd: "echo hi"},
		ID:   1,
	}
	canStart, devices := inv.CanStartGPUJob(state, job)
	if !canStart {
		t.Error("CPU-only job should always be startable")
	}
	if len(devices) != 0 {
		t.Errorf("expected no devices, got %v", devices)
	}
}

func TestCanStartGPUJob_ExplicitGPU(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100", TotalMemGB: 80},
		},
	}
	state := NewState()

	job := &RunnerJob{
		Data: &ops.CommandJob{ID: 1, Cmd: "train.py", GPU: "0"},
		ID:   1,
	}
	canStart, devices := inv.CanStartGPUJob(state, job)
	if !canStart {
		t.Error("should be able to start on free device")
	}
	if len(devices) != 1 || devices[0] != "0" {
		t.Errorf("expected [0], got %v", devices)
	}

	// Mark device in use
	state.AddRunning("99", RunningJobState{GPUDevices: []string{"0"}, GPUMemGB: 20})
	canStart, _ = inv.CanStartGPUJob(state, job)
	if canStart {
		t.Error("should not start when device is in use")
	}
}

func TestGetJobGPUDevices(t *testing.T) {
	tests := []struct {
		name string
		job  *ops.CommandJob
		want []string
	}{
		{"explicit GPU", &ops.CommandJob{GPU: "0,1"}, []string{"0", "1"}},
		{"env var", &ops.CommandJob{Env: []string{"CUDA_VISIBLE_DEVICES=2"}}, []string{"2"}},
		{"no GPU", &ops.CommandJob{}, nil},
		{"gpu field takes precedence", &ops.CommandJob{GPU: "1", Env: []string{"CUDA_VISIBLE_DEVICES=0"}}, []string{"1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetJobGPUDevices(tt.job)
			if len(got) != len(tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
				return
			}
			for i, v := range got {
				if v != tt.want[i] {
					t.Errorf("got[%d]=%q, want %q", i, v, tt.want[i])
				}
			}
		})
	}
}
