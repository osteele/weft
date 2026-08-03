package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/placement"
)

func TestValidatePinnedHostQueueGateRejectsDeterministicMismatch(t *testing.T) {
	restore := inventory.SetHosts([]inventory.HostSpec{
		{
			Name: "cool30",
			GPUs: []inventory.GPUSpec{
				{Name: "NVIDIA GeForce RTX 2080 Ti", Class: "rtx2080ti", Memory: "11GB", Indices: []int{0}},
			},
		},
	})
	t.Cleanup(restore)

	err := validatePinnedHostQueueGate("cool30", placement.Constraints{GPUClass: "ampere+"})
	if err == nil {
		t.Fatal("expected deterministic mismatch to be rejected")
	}
	if got, want := err.Error(), "gpu gate: no GPU matching class ampere+"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestValidatePinnedHostQueueGateAllowsUnknownHostData(t *testing.T) {
	restore := inventory.SetHosts([]inventory.HostSpec{
		{Name: "cool30"},
	})
	t.Cleanup(restore)

	if err := validatePinnedHostQueueGate("cool30", placement.Constraints{GPUClass: "ampere+"}); err != nil {
		t.Fatalf("expected unknown host GPU data to pass, got %v", err)
	}
}

func TestValidatePinnedHostQueueGateAllowsMatchingHost(t *testing.T) {
	restore := inventory.SetHosts([]inventory.HostSpec{
		{
			Name: "cool30",
			GPUs: []inventory.GPUSpec{
				{Name: "NVIDIA A100-PCIE-80GB", Class: "a100", Memory: "80GB", Indices: []int{0}},
			},
		},
	})
	t.Cleanup(restore)

	if err := validatePinnedHostQueueGate("cool30", placement.Constraints{GPUClass: "ampere+"}); err != nil {
		t.Fatalf("expected matching host to pass, got %v", err)
	}
}

func TestValidatePinnedHostQueueGateRejectsInsufficientMemory(t *testing.T) {
	restore := inventory.SetHosts([]inventory.HostSpec{
		{
			Name: "cool30",
			GPUs: []inventory.GPUSpec{
				{Name: "NVIDIA GeForce RTX 3090", Class: "rtx3090", Memory: "24576MiB", Indices: []int{0}},
			},
		},
	})
	t.Cleanup(restore)

	mem := 26
	err := validatePinnedHostQueueGate("cool30", placement.Constraints{GPUClass: "nvidia", GPUMemGB: mem})
	if err == nil {
		t.Fatal("expected insufficient memory to be rejected")
	}
	if got, want := err.Error(), "gpu gate: no GPU with >=26GB"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestValidatePinnedHostQueueGateRejectsRuntimeFloor(t *testing.T) {
	restore := inventory.SetHosts([]inventory.HostSpec{
		{
			Name:                "cool30",
			NVIDIADriverVersion: "525.125.06",
			CUDAVersion:         "12.0",
			GPUs: []inventory.GPUSpec{
				{Name: "NVIDIA GeForce RTX 3090", Class: "rtx3090", Memory: "24576MiB", Indices: []int{0}},
			},
		},
	})
	t.Cleanup(restore)

	err := validatePinnedHostQueueGate("cool30", placement.Constraints{
		GPUClass:         "nvidia",
		GPUMemGB:         20,
		MinCUDAVersion:   "12.8",
		MinDriverVersion: 570,
	})
	if err == nil {
		t.Fatal("expected runtime floor mismatch to be rejected")
	}
	if got := err.Error(); !strings.Contains(got, "driver floor: NVIDIA driver 525.125.06 < required >=570") {
		t.Fatalf("error = %q, want driver floor rejection", got)
	}
}

func TestValidatePinnedHostQueueGateRejectsHostAxes(t *testing.T) {
	inventory.UseTestHosts(t)

	tests := []struct {
		name        string
		constraints placement.Constraints
		want        string
	}{
		{
			name:        "cpu cores",
			constraints: placement.Constraints{CPUCores: 32},
			want:        "cpu gate: host CPU cores 16 below required 32",
		},
		{
			name:        "host ram",
			constraints: placement.Constraints{CPUMemGB: 128},
			want:        "memory gate: host RAM 64GB below required 128GB",
		},
		{
			name:        "interconnect",
			constraints: placement.Constraints{GPUClass: "nvidia", Interconnect: "nvlink"},
			want:        "interconnect gate: interconnect nvlink required; host GPU naming shows no match",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePinnedHostQueueGate("host-beta", tt.constraints)
			if err == nil {
				t.Fatal("expected pinned host gate rejection")
			}
			if got := err.Error(); got != tt.want {
				t.Fatalf("error = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidatePinnedHostQueueGateAllowsUnknownHostAxes(t *testing.T) {
	restore := inventory.SetHosts([]inventory.HostSpec{{Name: "unknown-host"}})
	t.Cleanup(restore)

	err := validatePinnedHostQueueGate("unknown-host", placement.Constraints{
		GPUClass: "a100",
		CPUCores: 64,
		CPUMemGB: 512,
	})
	if err != nil {
		t.Fatalf("expected unknown CPU/RAM and GPU data to pass, got %v", err)
	}
}

func TestValidatePinnedHostQueueGateIncompleteGPUDataStillRejectsHostAxis(t *testing.T) {
	restore := inventory.SetHosts([]inventory.HostSpec{{
		Name:     "partial",
		CPUCores: 16,
		GPUs:     []inventory.GPUSpec{{}},
	}})
	t.Cleanup(restore)

	err := validatePinnedHostQueueGate("partial", placement.Constraints{
		GPUClass: "a100",
		CPUCores: 64,
	})
	if err == nil {
		t.Fatal("expected CPU floor to reject despite incomplete GPU metadata")
	}
	if got, want := err.Error(), "cpu gate: host CPU cores 16 below required 64"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
