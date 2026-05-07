package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/inventory"
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

	err := validatePinnedHostQueueGate("cool30", "ampere+", nil)
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

	if err := validatePinnedHostQueueGate("cool30", "ampere+", nil); err != nil {
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

	if err := validatePinnedHostQueueGate("cool30", "ampere+", nil); err != nil {
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
	err := validatePinnedHostQueueGate("cool30", "nvidia", &mem)
	if err == nil {
		t.Fatal("expected insufficient memory to be rejected")
	}
	if got, want := err.Error(), "gpu gate: no GPU with >=26GB"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
