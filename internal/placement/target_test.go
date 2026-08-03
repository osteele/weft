package placement

import (
	"slices"
	"testing"

	"github.com/osteele/weft/internal/compat"
	"github.com/osteele/weft/internal/inventory"
)

func checkHostConstraintResult(host inventory.HostSpec, c Constraints) (bool, []string) {
	verdict := CheckHostConstraints(host, c)
	if !verdict.Eligible {
		return false, verdict.Messages()
	}
	return true, hostGPUPassReasons(host, c)
}

func TestEvaluateEligibilityHostParityWithCheckHostConstraints(t *testing.T) {
	inventory.UseTestHosts(t)
	tests := []struct {
		name string
		c    Constraints
	}{
		{name: "a100", c: Constraints{GPUClass: "a100"}},
		{name: "rtx3090", c: Constraints{GPUClass: "rtx3090"}},
		{name: "bare numeric normalizes", c: Constraints{GPUClass: "3090"}},
		{name: "nvidia family", c: Constraints{GPUClass: "nvidia"}},
		{name: "ampere generation", c: Constraints{GPUClass: "ampere"}},
		{name: "ampere plus generation", c: Constraints{GPUClass: "ampere+"}},
		{name: "turing generation", c: Constraints{GPUClass: "turing"}},
		{name: "memory floor", c: Constraints{GPUMemGB: 24}},
		{name: "matching count", c: Constraints{GPUClass: "a100", NumGPUs: 2}},
		{name: "insufficient count", c: Constraints{GPUClass: "a100", NumGPUs: 3}},
		{name: "arch floor", c: Constraints{GPUMemGB: 1, MinComputeCap: "7.5"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, host := range inventory.TestHosts() {
				oldEligible, _ := checkHostConstraintResult(host, tt.c)
				got := EvaluateEligibility(tt.c, TargetSpecFromHostSpec(host, nil, nil))
				if got.Eligible != oldEligible {
					t.Fatalf("%s: EvaluateEligibility = %v (%v), CheckHostConstraints = %v",
						host.Name, got.Eligible, got.Messages(), oldEligible)
				}
			}
		})
	}
}

func TestEvaluateEligibilityHostAxes(t *testing.T) {
	host := inventory.HostSpec{
		Name:     "host-beta",
		CPUCores: 16,
		Memory:   "64GB",
		GPUs: []inventory.GPUSpec{{
			Name:   "RTX 3090",
			Class:  "rtx3090",
			Memory: "24GB",
		}},
	}
	tests := []struct {
		name        string
		constraints Constraints
		wantKind    EligibilityReasonKind
	}{
		{
			name:        "cpu cores",
			constraints: Constraints{CPUCores: 32},
			wantKind:    ReasonCPUCores,
		},
		{
			name:        "host ram",
			constraints: Constraints{CPUMemGB: 128},
			wantKind:    ReasonHostRAM,
		},
		{
			name:        "interconnect",
			constraints: Constraints{GPUClass: "nvidia", Interconnect: "nvlink"},
			wantKind:    ReasonInterconnect,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verdict := EvaluateEligibility(tt.constraints, TargetSpecFromHostSpec(host, nil, nil))
			if verdict.Eligible {
				t.Fatal("EvaluateEligibility eligible, want host-axis rejection")
			}
			if !hasReason(verdict, tt.wantKind) {
				t.Fatalf("reasons = %+v, want %s", verdict.Reasons, tt.wantKind)
			}
		})
	}
}

func TestEvaluateEligibilityHostAxisUnknownCoresAndRAMPass(t *testing.T) {
	target := TargetSpecFromHostSpec(inventory.HostSpec{Name: "legacy"}, nil, nil)
	verdict := EvaluateEligibility(Constraints{CPUCores: 32, CPUMemGB: 128}, target)
	if !verdict.Eligible {
		t.Fatalf("EvaluateEligibility = ineligible (%v), want unknown host cores/RAM accepted", verdict.Messages())
	}
}

func TestEvaluateEligibilityGPUCountUsesIndividuallyMatchingDevices(t *testing.T) {
	host := inventory.HostSpec{
		Name: "mixed",
		GPUs: []inventory.GPUSpec{
			{Name: "A100 80GB PCIe", Class: "a100", Memory: "80GB", Indices: []int{0, 1}},
			{Name: "RTX 3090", Class: "rtx3090", Memory: "24GB", Indices: []int{2, 3}},
		},
	}
	verdict := EvaluateEligibility(Constraints{GPUClass: "a100", NumGPUs: 3}, TargetSpecFromHostSpec(host, nil, nil))
	if verdict.Eligible {
		t.Fatalf("EvaluateEligibility eligible, want count rejection")
	}
	if !hasReason(verdict, ReasonGPUCount) {
		t.Fatalf("reasons = %+v, want %s", verdict.Reasons, ReasonGPUCount)
	}
}

func TestEvaluateEligibilityFreeCapacityUsesReservations(t *testing.T) {
	host := inventory.HostSpec{
		Name: "reserved",
		GPUs: []inventory.GPUSpec{
			{Name: "A100 80GB PCIe", Class: "a100", Memory: "80GB", Indices: []int{0, 1}},
		},
	}
	reservations := []GPUReservation{{JobID: 10, Count: 2, GPUClass: "a100"}}
	target := TargetSpecFromHostSpec(host, nil, reservations)
	verdict := EvaluateEligibility(Constraints{GPUClass: "a100"}, target)
	if verdict.Eligible {
		t.Fatalf("EvaluateEligibility eligible, want reservation rejection")
	}
	if !hasReason(verdict, ReasonGPUAvailability) {
		t.Fatalf("reasons = %+v, want %s", verdict.Reasons, ReasonGPUAvailability)
	}

	self := Constraints{GPUClass: "a100", SelfJobID: 10}
	if got := EvaluateEligibility(self, target); !got.Eligible {
		t.Fatalf("self reservation should be ignored, got %v", got.Messages())
	}
}

func TestEvaluateEligibilityCUDAChainUsesUnifiedValidator(t *testing.T) {
	target := TargetSpec{
		Name:              "cuda-old",
		CUDAVersion:       "12.0",
		NVIDIADriverMajor: 550,
		Devices: []TargetDevice{
			TargetDeviceFromGPU("rtx4090", "RTX 4090", 24, 1, nil),
		},
	}
	constraints := Constraints{
		GPUClass:       "nvidia",
		MinCUDAVersion: "12.8",
		VersionRequirements: []compat.Requirement{{
			Axis:       compat.AxisGLIBCXX,
			Comparator: compat.ComparatorVersionMin,
		}},
	}
	verdict := EvaluateEligibility(constraints, target)
	if verdict.Eligible {
		t.Fatal("EvaluateEligibility eligible, want CUDA chain rejection")
	}
	if !hasReason(verdict, ReasonCUDAChain) {
		t.Fatalf("reasons = %+v, want %s", verdict.Reasons, ReasonCUDAChain)
	}
}

func TestEvaluateEligibilityMaxComputeCapUnknownFailsClosed(t *testing.T) {
	host := inventory.HostSpec{
		Name:        "future",
		CUDAVersion: "12.9",
		GPUs: []inventory.GPUSpec{{
			Name:   "Mystery Accelerator Z9",
			Class:  "mysteryz9",
			Memory: "80GB",
		}},
	}
	c := Constraints{MaxComputeCap: "9.0"}
	verdict := EvaluateEligibility(c, TargetSpecFromHostSpec(host, nil, nil))
	if verdict.Eligible {
		t.Fatalf("EvaluateEligibility = eligible, want unknown max cap to fail closed")
	}
	if !hasReason(verdict, ReasonComputeCapMax) {
		t.Fatalf("reasons = %+v, want %s", verdict.Reasons, ReasonComputeCapMax)
	}
	if ok, _ := checkHostConstraintResult(host, c); ok {
		t.Fatalf("CheckHostConstraints = eligible, want wrapper to follow EvaluateEligibility")
	}
}

func TestEvaluateEligibilityMinComputeCapUnknownFailsClosed(t *testing.T) {
	target := TargetSpec{
		Name: "unknown-min",
		Devices: []TargetDevice{{
			Name:     "Mystery Accelerator Z9",
			Class:    "mysteryz9",
			Family:   "nvidia",
			MemoryGB: 80,
			Count:    1,
		}},
	}
	verdict := EvaluateEligibility(Constraints{GPUMemGB: 1, MinComputeCap: "7.5"}, target)
	if verdict.Eligible {
		t.Fatal("EvaluateEligibility eligible, want min-cap unknown rejection")
	}
	if !hasReason(verdict, ReasonComputeCapMin) {
		t.Fatalf("reasons = %+v, want %s", verdict.Reasons, ReasonComputeCapMin)
	}
}

func hasReason(v Verdict, kind EligibilityReasonKind) bool {
	return slices.ContainsFunc(v.Reasons, func(r EligibilityReason) bool {
		return r.Kind == kind
	})
}
