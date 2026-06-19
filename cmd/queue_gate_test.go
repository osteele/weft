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
