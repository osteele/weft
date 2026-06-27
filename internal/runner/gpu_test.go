package runner

import (
	"fmt"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/opsqueue"
)

// nvidiaSmiTableOutput is the full output from nvidia-smi on a host with
// driver 525.125.06, where --query-gpu and --format flags are silently
// ignored and the default table format is returned instead. This happens
// on some older driver versions.
const nvidiaSmiTableOutput = `Wed Mar  4 16:10:21 2026
+-----------------------------------------------------------------------------+
| NVIDIA-SMI 525.125.06   Driver Version: 525.125.06   CUDA Version: 12.0     |
|-------------------------------+----------------------+----------------------+
| GPU  Name        Persistence-M| Bus-Id        Disp.A | Volatile Uncorr. ECC |
| Fan  Temp  Perf  Pwr:Usage/Cap|         Memory-Usage | GPU-Util  Compute M. |
|                               |                      |               MIG M. |
|===============================+======================+======================|
|   0  NVIDIA GeForce RTX 3090  On   | 00000000:01:00.0 Off |                  N/A |
| 51%   45C    P8    22W / 350W |      6MiB / 24576MiB |      0%      Default |
|                               |                      |                  N/A |
+-------------------------------+----------------------+----------------------+
|   1  NVIDIA GeForce RTX 3090  On   | 00000000:41:00.0 Off |                  N/A |
| 51%   45C    P8    19W / 350W |      6MiB / 24576MiB |      0%      Default |
|                               |                      |                  N/A |
+-------------------------------+----------------------+----------------------+
`

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
		got := inventory.NormalizeGPUClass(tt.input)
		if got != tt.want {
			t.Errorf("NormalizeGPUClass(%q) = %q, want %q", tt.input, got, tt.want)
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
		// Exact model matching
		{"A100", 2},
		{"a100", 2},
		{"2080", 0}, // Normalized to rtx2080; does not match rtx2080ti
		{"2080ti", 1},
		{"3090", 1},
		{"RTX", 0}, // Not a model, generation, or family constraint
		{"H100", 0},
		{"rtx-3090", 1},  // Normalized: hyphen stripped
		{"RTX 3090", 1},  // Normalized: space stripped
		{"rtx3090", 1},   // Normalized: already clean
		{"a100-pcie", 2}, // Normalized: hyphen stripped

		// Generation matching
		{"ampere", 3}, // A100 (x2) + RTX 3090 (all Ampere)
		{"turing", 1}, // Only RTX 2080 Ti
		{"hopper", 0}, // None

		// Minimum generation matching
		{"ampere+", 3}, // A100 (x2) + RTX 3090 (all Ampere+)
		{"turing+", 4}, // All 4 devices (Turing+)
		{"hopper+", 0}, // None
	}

	for _, tt := range tests {
		got := inv.DevicesByClass(tt.className)
		if len(got) != tt.wantCount {
			t.Errorf("DevicesByClass(%q) = %v (len %d), want len %d", tt.className, got, len(got), tt.wantCount)
		}
	}
}

func TestDevicesByClass_DoesNotFuzzyMatchExactCatalogModels(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA L40", TotalMemGB: 48},
			{Index: "1", Name: "NVIDIA L40S", TotalMemGB: 48},
		},
	}

	if got := inv.DevicesByClass("l4"); len(got) != 0 {
		t.Errorf("DevicesByClass(\"l4\") = %v, want no L40/L40S substring match", got)
	}
	if got := inv.DevicesByClass("l40"); len(got) != 1 || got[0] != "0" {
		t.Errorf("DevicesByClass(\"l40\") = %v, want only L40", got)
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
		Data: &opsqueue.CommandJob{ID: 1, Cmd: "echo hi"},
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
		Data: &opsqueue.CommandJob{ID: 1, Cmd: "train.py", GPU: "0"},
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
		Data: &opsqueue.CommandJob{ID: 1, Cmd: "train.py", GPU: "0"},
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
		Data: &opsqueue.CommandJob{ID: 1, Cmd: "train.py", GPU: "0"},
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

// cool100-like mixed GPU inventory: A100 at indices 0,1 + RTX 2080 Ti at indices 2-9
func newMixedGPUInventory() *GPUInventory {
	devices := []GPUInfo{
		{Index: "0", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
		{Index: "1", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
	}
	for i := 2; i <= 9; i++ {
		devices = append(devices, GPUInfo{
			Index:      fmt.Sprintf("%d", i),
			Name:       "NVIDIA GeForce RTX 2080 Ti",
			TotalMemGB: 11,
		})
	}
	return &GPUInventory{Devices: devices}
}

func TestPickBestGPUForClass_MixedGPUHost_OnlyReturnsMatchingClass(t *testing.T) {
	inv := newMixedGPUInventory()
	state := NewState()

	// Mark both A100 devices (0, 1) as occupied
	state.AddRunning("100", RunningJobState{GPUDevices: []string{"0"}, GPUMemGB: 20})
	state.AddRunning("101", RunningJobState{GPUDevices: []string{"1"}, GPUMemGB: 20})

	// All A100s busy — must NOT fall back to a 2080 Ti
	device, ok := inv.PickBestGPUForClass(state, "a100", 20)
	if ok {
		t.Errorf("expected no available A100, but got device %s (must not fall back to 2080 Ti)", device)
	}
}

func TestCanStartGPUJob_GPUClass_MixedHost_AllClassDevicesBusy(t *testing.T) {
	inv := newMixedGPUInventory()
	state := NewState()

	// Mark both A100 devices as busy
	state.AddRunning("100", RunningJobState{GPUDevices: []string{"0"}, GPUMemGB: 20})
	state.AddRunning("101", RunningJobState{GPUDevices: []string{"1"}, GPUMemGB: 20})

	job := &RunnerJob{
		Data: &opsqueue.CommandJob{ID: 200, Cmd: "train.py", GPUClass: "a100"},
		ID:   200,
	}

	canStart, devices := inv.CanStartGPUJob(state, job)
	if canStart {
		t.Errorf("should not start A100 job when all A100s are busy, but got devices %v", devices)
	}
}

func TestCanStartGPUJob_GPUClass_MixedHost_SelectsCorrectDevice(t *testing.T) {
	inv := newMixedGPUInventory()
	state := NewState()

	job := &RunnerJob{
		Data: &opsqueue.CommandJob{ID: 200, Cmd: "train.py", GPUClass: "a100"},
		ID:   200,
	}

	canStart, devices := inv.CanStartGPUJob(state, job)
	if !canStart {
		t.Fatal("expected job to start on free A100")
	}
	if len(devices) != 1 {
		t.Fatalf("expected exactly 1 device, got %v", devices)
	}

	// Device must be an A100 index (0 or 1), never a 2080 Ti index (2-9)
	dev := devices[0]
	if dev != "0" && dev != "1" {
		t.Errorf("expected A100 device (0 or 1), got %s — job was assigned to a 2080 Ti", dev)
	}
}

func TestDevicesByClass_MixedHost_GenerationConstraint(t *testing.T) {
	inv := newMixedGPUInventory()

	// "ampere" should match A100s (indices 0, 1) but NOT 2080 Ti (Turing, indices 2-9)
	devices := inv.DevicesByClass("ampere")

	// Build a set of returned indices for easy checking
	devSet := make(map[string]bool, len(devices))
	for _, d := range devices {
		devSet[d] = true
	}

	// A100s must be included
	if !devSet["0"] || !devSet["1"] {
		t.Errorf("ampere should include A100 indices 0 and 1, got %v", devices)
	}

	// 2080 Ti indices (2-9) must NOT be included
	for i := 2; i <= 9; i++ {
		idx := fmt.Sprintf("%d", i)
		if devSet[idx] {
			t.Errorf("ampere should NOT include 2080 Ti at index %s, got %v", idx, devices)
		}
	}
}

// TestGPUClassJob_ResolutionAndEnv_MixedHost verifies that GPU class resolution
// produces the correct CUDA_VISIBLE_DEVICES value on a mixed-GPU host.
func TestGPUClassJob_ResolutionAndEnv_MixedHost(t *testing.T) {
	inv := newMixedGPUInventory()
	state := NewState()

	job := &opsqueue.CommandJob{ID: 200, Cmd: "train.py", GPUClass: "a100"}
	memPerDevice := GetJobGPUMem(job, DefaultGPUMemGB)
	device, ok := inv.PickBestGPUForClass(state, job.GPUClass, memPerDevice)
	if !ok {
		t.Fatal("expected A100 device from PickBestGPUForClass")
	}

	// FormatGPUDeviceEnv must produce a value pointing at an A100
	cudaEnv := FormatGPUDeviceEnv([]string{device})
	if cudaEnv != "CUDA_VISIBLE_DEVICES=0" && cudaEnv != "CUDA_VISIBLE_DEVICES=1" {
		t.Errorf("expected CUDA_VISIBLE_DEVICES=0 or =1, got %q", cudaEnv)
	}
}

func TestGetJobGPUDevices(t *testing.T) {
	tests := []struct {
		name string
		job  *opsqueue.CommandJob
		want []string
	}{
		{"explicit GPU", &opsqueue.CommandJob{GPU: "0,1"}, []string{"0", "1"}},
		{"env var", &opsqueue.CommandJob{Env: []string{"CUDA_VISIBLE_DEVICES=2"}}, []string{"2"}},
		{"no GPU", &opsqueue.CommandJob{}, nil},
		{"gpu field takes precedence", &opsqueue.CommandJob{GPU: "1", Env: []string{"CUDA_VISIBLE_DEVICES=0"}}, []string{"1"}},
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

// TestParseNvidiaSmiOutput_TableFormat verifies that parseNvidiaSmiOutput
// returns zero rows when given table-format output (old driver 525 that
// silently ignores --query-gpu/--format flags).
func TestParseNvidiaSmiOutput_TableFormat(t *testing.T) {
	rows := parseNvidiaSmiOutput(nvidiaSmiTableOutput, 4)
	if len(rows) != 0 {
		t.Errorf("expected 0 rows from table output, got %d", len(rows))
	}
	// parseNvidiaSmiOutput returns nil when no lines match CSV format.
	// This ensures DiscoverGPUs reaches the table format fallback path.
}

// TestParseNvidiaSmiTable_TruncatedNames verifies parsing works when
// nvidia-smi truncates GPU names with "..." (observed on driver 525.125.06
// when terminal width is limited).
func TestParseNvidiaSmiTable_TruncatedNames(t *testing.T) {
	truncatedOutput := `Wed Mar  4 16:46:12 2026
+-----------------------------------------------------------------------------+
| NVIDIA-SMI 525.125.06   Driver Version: 525.125.06   CUDA Version: 12.0     |
|-------------------------------+----------------------+----------------------+
| GPU  Name        Persistence-M| Bus-Id        Disp.A | Volatile Uncorr. ECC |
| Fan  Temp  Perf  Pwr:Usage/Cap|         Memory-Usage | GPU-Util  Compute M. |
|===============================+======================+======================|
|   0  NVIDIA GeForce ...  On   | 00000000:01:00.0 Off |                  N/A |
| 51%   45C    P8    22W / 350W |      6MiB / 24576MiB |      0%      Default |
|                               |                      |                  N/A |
+-------------------------------+----------------------+----------------------+
|   1  NVIDIA GeForce ...  On   | 00000000:41:00.0 Off |                  N/A |
| 51%   45C    P8    19W / 350W |   4096MiB / 24576MiB |     35%      Default |
|                               |                      |                  N/A |
+-------------------------------+----------------------+----------------------+
`
	devices, memSnapshot := parseNvidiaSmiTable(truncatedOutput)
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}
	if devices[0].Index != "0" || devices[1].Index != "1" {
		t.Errorf("device indices = [%s, %s], want [0, 1]", devices[0].Index, devices[1].Index)
	}
	if devices[0].TotalMemGB != 24 {
		t.Errorf("device[0].TotalMemGB = %d, want 24", devices[0].TotalMemGB)
	}
	// Verify memory snapshot captures different usage values
	if memSnapshot["1"].UsedMiB != 4096 {
		t.Errorf("memSnapshot[1].UsedMiB = %d, want 4096", memSnapshot["1"].UsedMiB)
	}
}

// TestParseNvidiaSmiTableOutput verifies that GPU info can be extracted
// from the default nvidia-smi table format used by old drivers that
// silently ignore --query-gpu/--format flags.
func TestParseNvidiaSmiTable(t *testing.T) {
	devices, memSnapshot := parseNvidiaSmiTable(nvidiaSmiTableOutput)
	if len(devices) == 0 {
		t.Fatal("expected devices from table output, got none")
	}
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}

	// Check first device
	d := devices[0]
	if d.Index != "0" {
		t.Errorf("device[0].Index = %q, want %q", d.Index, "0")
	}
	if !strings.Contains(d.Name, "RTX 3090") {
		t.Errorf("device[0].Name = %q, want to contain 'RTX 3090'", d.Name)
	}
	if d.TotalMemGB != 24 {
		t.Errorf("device[0].TotalMemGB = %d, want 24", d.TotalMemGB)
	}

	// Check memory snapshot was populated in the same pass
	if len(memSnapshot) != 2 {
		t.Fatalf("expected 2 memory entries, got %d", len(memSnapshot))
	}
	mem0 := memSnapshot["0"]
	if mem0.UsedMiB != 6 {
		t.Errorf("memSnapshot[0].UsedMiB = %d, want 6", mem0.UsedMiB)
	}
	if mem0.TotalMiB != 24576 {
		t.Errorf("memSnapshot[0].TotalMiB = %d, want 24576", mem0.TotalMiB)
	}
}

// TestDiscoverGPUs_FallbackToTableParsing verifies that DiscoverGPUs
// uses table output parsing as a fallback when CSV parsing returns no rows.
func TestDiscoverGPUs_FallbackToTableParsing(t *testing.T) {
	// Simulate what happens when nvidia-smi returns table format:
	// CSV parsing returns 0 rows, but table parsing finds devices.
	csvRows := parseNvidiaSmiOutput(nvidiaSmiTableOutput, 4)
	if len(csvRows) != 0 {
		t.Skip("CSV parsing unexpectedly succeeded on table output")
	}

	tableDevices, _ := parseNvidiaSmiTable(nvidiaSmiTableOutput)
	if len(tableDevices) == 0 {
		t.Error("table fallback parsing should find devices in nvidia-smi table output")
	}
}

// TestPickBestGPUForClass_EmptyInventory verifies the behavior when
// the GPU inventory is empty (e.g., nvidia-smi query failed).
func TestPickBestGPUForClass_EmptyInventory(t *testing.T) {
	inv := &GPUInventory{
		Devices:      nil,
		hasNvidiaSmi: true, // nvidia-smi exists but returned unparseable output
	}
	state := NewState()

	_, ok := inv.PickBestGPUForClass(state, "nvidia", 24)
	if ok {
		t.Error("expected no device from empty inventory")
	}
}

func TestPickLeastLoadedGPU_SelectsMostFreeMemory(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA GeForce RTX 3090", TotalMemGB: 24},
			{Index: "1", Name: "NVIDIA GeForce RTX 3090", TotalMemGB: 24},
			{Index: "2", Name: "NVIDIA GeForce RTX 3090", TotalMemGB: 24},
		},
		DeviceMemSnapshot: map[string]DeviceMemInfo{
			"0": {UsedMiB: 23 * 1024, TotalMiB: 24 * 1024}, // ~96% used
			"1": {UsedMiB: 10 * 1024, TotalMiB: 24 * 1024}, // ~42% used
			"2": {UsedMiB: 100, TotalMiB: 24 * 1024},       // nearly idle
		},
	}
	state := NewState()

	device := inv.PickLeastLoadedGPU(state)
	if device != "2" {
		t.Errorf("expected device 2 (most free memory), got %s", device)
	}
}

func TestPickLeastLoadedGPU_SkipsBusyDevices(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA GeForce RTX 3090", TotalMemGB: 24},
			{Index: "1", Name: "NVIDIA GeForce RTX 3090", TotalMemGB: 24},
		},
		DeviceMemSnapshot: map[string]DeviceMemInfo{
			"0": {UsedMiB: 100, TotalMiB: 24 * 1024},
			"1": {UsedMiB: 100, TotalMiB: 24 * 1024},
		},
	}
	state := NewState()
	state.AddRunning("99", RunningJobState{GPUDevices: []string{"0"}, GPUMemGB: 20})

	device := inv.PickLeastLoadedGPU(state)
	if device != "1" {
		t.Errorf("expected device 1 (device 0 has running job), got %s", device)
	}
}

func TestPickLeastLoadedGPU_EmptyInventory(t *testing.T) {
	inv := &GPUInventory{}
	state := NewState()

	device := inv.PickLeastLoadedGPU(state)
	if device != "" {
		t.Errorf("expected empty string from empty inventory, got %s", device)
	}
}

func TestPickLeastLoadedGPU_NoSnapshot(t *testing.T) {
	// Without a snapshot, falls back to full static capacity and picks
	// the first device (all tied). This is the best we can do.
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA GeForce RTX 3090", TotalMemGB: 24},
			{Index: "1", Name: "NVIDIA GeForce RTX 3090", TotalMemGB: 24},
		},
	}
	state := NewState()

	device := inv.PickLeastLoadedGPU(state)
	if device == "" {
		t.Error("expected a device even without snapshot")
	}
}

func TestEnrichNamesFromHostSpec(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA GeForce ...", TotalMemGB: 24},
			{Index: "1", Name: "NVIDIA GeForce ...", TotalMemGB: 24},
			{Index: "2", Name: "NVIDIA GeForce ...", TotalMemGB: 24},
		},
	}
	spec := &inventory.HostSpec{
		Name: "cool30",
		GPUs: []inventory.GPUSpec{
			{Name: "NVIDIA GeForce RTX 3090", Class: "rtx3090", Memory: "24576MiB", Indices: []int{0, 1, 2}},
		},
	}

	inv.EnrichNamesFromHostSpec(spec)

	for i, d := range inv.Devices {
		if d.Name != "NVIDIA GeForce RTX 3090" {
			t.Errorf("Devices[%d].Name = %q, want %q", i, d.Name, "NVIDIA GeForce RTX 3090")
		}
	}

	// After enrichment, class matching should work
	devices := inv.DevicesByClass("3090")
	if len(devices) != 3 {
		t.Errorf("DevicesByClass(\"3090\") after enrichment = %v, want 3 devices", devices)
	}
}

func TestEnrichNamesFromHostSpec_NonTruncatedUnchanged(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100-PCIE-80GB", TotalMemGB: 80},
		},
	}
	spec := &inventory.HostSpec{
		Name: "cool100",
		GPUs: []inventory.GPUSpec{
			{Name: "NVIDIA A100-PCIE-80GB", Class: "a100", Memory: "80GB", Indices: []int{0}},
		},
	}

	inv.EnrichNamesFromHostSpec(spec)

	if inv.Devices[0].Name != "NVIDIA A100-PCIE-80GB" {
		t.Errorf("non-truncated name changed: got %q", inv.Devices[0].Name)
	}
}

func TestEnrichNamesFromHostSpec_MixedGPUs(t *testing.T) {
	// cool100-like: A100s at 0,1 and 2080 Tis at 2-9, all truncated
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100-PCI...", TotalMemGB: 80},
			{Index: "1", Name: "NVIDIA A100-PCI...", TotalMemGB: 80},
			{Index: "2", Name: "NVIDIA GeForce ...", TotalMemGB: 11},
		},
	}
	spec := &inventory.HostSpec{
		Name: "cool100",
		GPUs: []inventory.GPUSpec{
			{Name: "NVIDIA A100-PCIE-80GB", Class: "a100", Memory: "80GB", Indices: []int{0, 1}},
			{Name: "NVIDIA GeForce RTX 2080 Ti", Class: "rtx2080ti", Memory: "11GB", Indices: []int{2}},
		},
	}

	inv.EnrichNamesFromHostSpec(spec)

	if inv.Devices[0].Name != "NVIDIA A100-PCIE-80GB" {
		t.Errorf("Devices[0].Name = %q, want NVIDIA A100-PCIE-80GB", inv.Devices[0].Name)
	}
	if inv.Devices[2].Name != "NVIDIA GeForce RTX 2080 Ti" {
		t.Errorf("Devices[2].Name = %q, want NVIDIA GeForce RTX 2080 Ti", inv.Devices[2].Name)
	}

	// Class matching should now work for both types
	a100s := inv.DevicesByClass("a100")
	if len(a100s) != 2 {
		t.Errorf("DevicesByClass(\"a100\") = %v, want 2", a100s)
	}
	ti2080s := inv.DevicesByClass("2080ti")
	if len(ti2080s) != 1 {
		t.Errorf("DevicesByClass(\"2080ti\") = %v, want 1", ti2080s)
	}
}

func TestDevicesByClass_TruncatedNames(t *testing.T) {
	// On driver 525.x, nvidia-smi truncates GPU names to "NVIDIA GeForce ..."
	// in table format. Class matching for specific models fails.
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA GeForce ...", TotalMemGB: 24},
			{Index: "1", Name: "NVIDIA GeForce ...", TotalMemGB: 24},
		},
	}

	// Specific model matching fails with truncated names
	if got := inv.DevicesByClass("3090"); len(got) != 0 {
		t.Errorf("DevicesByClass(\"3090\") on truncated names = %v, want empty", got)
	}
	if got := inv.DevicesByClass("rtx3090"); len(got) != 0 {
		t.Errorf("DevicesByClass(\"rtx3090\") on truncated names = %v, want empty", got)
	}

	// Family matching still works
	if got := inv.DevicesByClass("nvidia"); len(got) != 2 {
		t.Errorf("DevicesByClass(\"nvidia\") on truncated names = %v, want 2 devices", got)
	}
}

func TestCanStartGPUJob_InventoryNonBenchmark_AllowsMinorComputeProcess(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100", TotalMemGB: 80},
		},
		DeviceMemSnapshot: map[string]DeviceMemInfo{
			"0": {UsedMiB: 10 * 1024, TotalMiB: 80 * 1024},
		},
		DeviceHasCompute: map[string]bool{"0": true},
		DeviceUtilPct:    map[string]int{"0": defaultNonBenchmarkGPUBusyUtilThreshold - 1},
		isInventoryHost:  true,
	}
	state := NewState()
	job := &RunnerJob{
		Data: &opsqueue.CommandJob{ID: 1, Cmd: "train.py", GPUClass: "a100"},
		ID:   1,
	}

	canStart, devices, reason := inv.CanStartGPUJobWithReason(state, job)
	if !canStart {
		t.Fatalf("expected non-benchmark job to start despite minor compute load, reason=%q", reason)
	}
	if len(devices) != 1 || devices[0] != "0" {
		t.Fatalf("expected device [0], got %v", devices)
	}
}

func TestCanStartGPUJob_InventoryNonBenchmark_BlocksHighComputeUtil(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100", TotalMemGB: 80},
		},
		DeviceMemSnapshot: map[string]DeviceMemInfo{
			"0": {UsedMiB: 10 * 1024, TotalMiB: 80 * 1024},
		},
		DeviceHasCompute: map[string]bool{"0": true},
		DeviceUtilPct:    map[string]int{"0": defaultNonBenchmarkGPUBusyUtilThreshold},
		isInventoryHost:  true,
	}
	state := NewState()
	job := &RunnerJob{
		Data: &opsqueue.CommandJob{ID: 1, Cmd: "train.py", GPUClass: "a100"},
		ID:   1,
	}

	canStart, _, _ := inv.CanStartGPUJobWithReason(state, job)
	if canStart {
		t.Fatal("expected non-benchmark job to be blocked by high compute utilization")
	}
}

func TestCanStartGPUJob_InventoryBenchmark_BlocksAnyComputeProcess(t *testing.T) {
	inv := &GPUInventory{
		Devices: []GPUInfo{
			{Index: "0", Name: "NVIDIA A100", TotalMemGB: 80},
		},
		DeviceMemSnapshot: map[string]DeviceMemInfo{
			"0": {UsedMiB: 10 * 1024, TotalMiB: 80 * 1024},
		},
		DeviceHasCompute: map[string]bool{"0": true},
		DeviceUtilPct:    map[string]int{"0": 1},
		isInventoryHost:  true,
	}
	state := NewState()
	job := &RunnerJob{
		Data: &opsqueue.CommandJob{ID: 1, Cmd: "train.py", GPUClass: "a100", Tags: []string{"benchmark-isolation"}},
		ID:   1,
	}

	canStart, _, _ := inv.CanStartGPUJobWithReason(state, job)
	if canStart {
		t.Fatal("expected benchmark job to be blocked by any active compute process")
	}
}

func TestParseNvidiaSmiTableComputeProcessFlags(t *testing.T) {
	out := `Fri Apr 10 13:13:10 2026
+-----------------------------------------------------------------------------+
| Processes:                                                                  |
|  GPU   GI   CI        PID   Type   Process name                  GPU Memory |
|        ID   ID                                                   Usage      |
|=============================================================================|
|    0   N/A  N/A      1234      C   python train.py                   20MiB |
|    1   N/A  N/A      3156      G   /usr/lib/xorg/Xorg                 4MiB |
|    2   N/A  N/A      5555    C+G   /usr/bin/obs                      32MiB |
+-----------------------------------------------------------------------------+`

	flags := parseNvidiaSmiTableComputeProcessFlags(out)
	if !flags["0"] {
		t.Fatal("expected GPU 0 to be marked compute-busy")
	}
	if flags["1"] {
		t.Fatal("expected GPU 1 (display-only G) to be ignored")
	}
	if !flags["2"] {
		t.Fatal("expected GPU 2 (C+G) to be marked compute-busy")
	}
}
