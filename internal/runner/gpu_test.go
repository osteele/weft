package runner

import (
	"testing"

	"github.com/osteele/weft/internal/ops"
)

func TestParseNvidiaSmiOutput(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		fieldCount int
		wantRows   int
		wantFirst  []string
	}{
		{
			name:       "standard 3-field output",
			out:        "0, NVIDIA A100-PCIE-80GB, 81920\n1, NVIDIA A100-PCIE-80GB, 81920\n",
			fieldCount: 3,
			wantRows:   2,
			wantFirst:  []string{"0", "NVIDIA A100-PCIE-80GB", "81920"},
		},
		{
			name:       "single field",
			out:        "42\n95\n",
			fieldCount: 1,
			wantRows:   2,
			wantFirst:  []string{"42"},
		},
		{
			name:       "empty output",
			out:        "",
			fieldCount: 3,
			wantRows:   0,
		},
		{
			name:       "blank lines skipped",
			out:        "\n0, name, 1024\n\n",
			fieldCount: 3,
			wantRows:   1,
			wantFirst:  []string{"0", "name", "1024"},
		},
		{
			name:       "short line skipped",
			out:        "0, name\n0, name, 1024\n",
			fieldCount: 3,
			wantRows:   1,
			wantFirst:  []string{"0", "name", "1024"},
		},
		{
			name:       "whitespace trimmed",
			out:        "  0 ,  NVIDIA A100 ,  81920 \n",
			fieldCount: 3,
			wantRows:   1,
			wantFirst:  []string{"0", "NVIDIA A100", "81920"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := parseNvidiaSmiOutput(tt.out, tt.fieldCount)
			if len(rows) != tt.wantRows {
				t.Fatalf("got %d rows, want %d", len(rows), tt.wantRows)
			}
			if tt.wantFirst != nil && len(rows) > 0 {
				for i, want := range tt.wantFirst {
					if rows[0][i] != want {
						t.Errorf("rows[0][%d] = %q, want %q", i, rows[0][i], want)
					}
				}
			}
		})
	}
}

func TestNormalizeGPUClass(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"RTX 3090", "rtx3090"},
		{"rtx-3090", "rtx3090"},
		{"rtx3090", "rtx3090"},
		{"A100", "a100"},
		{"NVIDIA A100-PCIE-80GB", "nvidiaa100pcie80gb"},
	}
	for _, tt := range tests {
		got := normalizeGPUClass(tt.input)
		if got != tt.want {
			t.Errorf("normalizeGPUClass(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

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
		{"rtx-3090", 1},  // Normalized: hyphen stripped
		{"RTX 3090", 1},  // Normalized: space stripped
		{"rtx3090", 1},   // Normalized: already clean
		{"a100-pcie", 2}, // Normalized: hyphen stripped
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

func TestPickBestGPUForClass_ActualMemory(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
			{Index: "1", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
		},
		// Device 0 has 60GB used by an external process, only 20GB free
		DeviceMemSnapshot: map[string]DeviceMemInfo{
			"0": {UsedMiB: 60 * 1024, TotalMiB: 80 * 1024},
			"1": {UsedMiB: 5 * 1024, TotalMiB: 80 * 1024},
		},
	}
	state := NewState()

	// Job needs 30GB — device 0 should be rejected (only 20GB free), device 1 picked
	device, ok := inv.PickBestGPUForClass(state, "A100", 30)
	if !ok {
		t.Fatal("expected a device")
	}
	if device != "1" {
		t.Errorf("expected device 1 (75GB free), got %s", device)
	}
}

func TestPickBestGPUForClass_AllDevicesFull(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
			{Index: "1", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
		},
		// Both devices have external processes consuming most VRAM
		DeviceMemSnapshot: map[string]DeviceMemInfo{
			"0": {UsedMiB: 75 * 1024, TotalMiB: 80 * 1024},
			"1": {UsedMiB: 70 * 1024, TotalMiB: 80 * 1024},
		},
	}
	state := NewState()

	// Job needs 20GB — neither device has enough actual free memory
	_, ok := inv.PickBestGPUForClass(state, "A100", 20)
	if ok {
		t.Error("expected no device available (both full from external processes)")
	}
}

func TestPickBestGPUForClass_NoSnapshot(t *testing.T) {
	// When no snapshot is available, should fall back to reservation-only checks
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
		},
	}
	state := NewState()

	device, ok := inv.PickBestGPUForClass(state, "A100", 40)
	if !ok {
		t.Fatal("expected device when no snapshot available")
	}
	if device != "0" {
		t.Errorf("expected 0, got %s", device)
	}
}

func TestCanStartGPUJob_ExplicitGPU_ActualMemory(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100", TotalMemGB: 80},
		},
		// Device 0 has external memory pressure — only 10GB free
		DeviceMemSnapshot: map[string]DeviceMemInfo{
			"0": {UsedMiB: 70 * 1024, TotalMiB: 80 * 1024},
		},
	}
	state := NewState()

	job := &RunnerJob{
		Data: &ops.CommandJob{ID: 1, Cmd: "train.py", GPU: "0"},
		ID:   1,
	}
	// Default GPU mem is 20GB, device only has 10GB free → rejected
	canStart, _ := inv.CanStartGPUJob(state, job)
	if canStart {
		t.Error("should not start when device has insufficient actual VRAM")
	}
}

func TestCanStartGPUJob_ExplicitGPU_ActualMemory_OK(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100", TotalMemGB: 80},
		},
		// Device 0 has plenty of free memory
		DeviceMemSnapshot: map[string]DeviceMemInfo{
			"0": {UsedMiB: 10 * 1024, TotalMiB: 80 * 1024},
		},
	}
	state := NewState()

	job := &RunnerJob{
		Data: &ops.CommandJob{ID: 1, Cmd: "train.py", GPU: "0"},
		ID:   1,
	}
	canStart, devices := inv.CanStartGPUJob(state, job)
	if !canStart {
		t.Error("should be able to start with enough actual VRAM")
	}
	if len(devices) != 1 || devices[0] != "0" {
		t.Errorf("expected [0], got %v", devices)
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
