package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/placement"
)

func TestTargetSpecFromOfferPreservesCloudGPUClassCompatibility(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		gpuName   string
	}{
		{name: "a100 sxm variant", requested: "a100", gpuName: "A100 SXM4"},
		{name: "rtx4090 punctuation", requested: "rtx4090", gpuName: "RTX_4090"},
		{name: "bare numeric alias", requested: "3090", gpuName: "RTX 3090"},
		{name: "broad nvidia", requested: "nvidia", gpuName: "RTX 4090"},
		{name: "ampere minimum generation", requested: "ampere+", gpuName: "RTX A6000"},
		{name: "v100 tesla alias", requested: "v100", gpuName: "Tesla V100"},
		{name: "a6000 rtx alias", requested: "a6000", gpuName: "RTX A6000"},
		{name: "gh200 grace hopper alias", requested: "gh200", gpuName: "Grace Hopper"},
		{name: "rtx4080 admits super variant", requested: "rtx-4080", gpuName: "RTX 4080S"},
		{name: "rtx4080 super admits short provider spelling", requested: "rtx-4080-super", gpuName: "RTX 4080S"},
		{name: "rtx4080 super admits long provider spelling", requested: "rtx-4080-super", gpuName: "RTX 4080 SUPER"},
		{name: "h100 broad admits pcie variant", requested: "h100", gpuName: "H100 PCIE"},
		{name: "h100 broad admits sxm variant", requested: "h100", gpuName: "H100 SXM"},
		{name: "h100 pcie pins pcie variant", requested: "h100-pcie", gpuName: "H100 PCIE"},
		{name: "h100 sxm pins sxm variant", requested: "h100-sxm", gpuName: "H100 SXM"},
		{name: "h100 hbm3 aliases sxm variant", requested: "h100-hbm3", gpuName: "H100 SXM"},
		{name: "unknown vastai still satisfies broad nvidia", requested: "nvidia", gpuName: "Future Accelerator Z9"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := TargetSpecFromOffer(cloud.Offer{
				Provider:   cloud.ProviderVastai,
				ProviderID: tt.name,
				GPUName:    tt.gpuName,
				GPUMemGB:   24,
				NumGPUs:    1,
			})
			got := placement.EvaluateEligibility(placement.Constraints{GPUClass: tt.requested}, target).Eligible
			if !got {
				t.Fatalf("EvaluateEligibility rejected %q vs %q", tt.requested, tt.gpuName)
			}
		})
	}
}

func TestTargetSpecFromOfferRejectsMismatchedH100Variant(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		gpuName   string
	}{
		{name: "pcie excludes sxm", requested: "h100-pcie", gpuName: "H100 SXM"},
		{name: "sxm excludes pcie", requested: "h100-sxm", gpuName: "H100 PCIE"},
		{name: "hbm3 excludes pcie", requested: "h100-hbm3", gpuName: "H100 PCIE"},
		{name: "nvl excludes sxm", requested: "h100-nvl", gpuName: "H100 SXM"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := TargetSpecFromOffer(cloud.Offer{
				Provider:   cloud.ProviderVastai,
				ProviderID: tt.name,
				GPUName:    tt.gpuName,
				GPUMemGB:   80,
				NumGPUs:    1,
			})
			if got := placement.EvaluateEligibility(placement.Constraints{GPUClass: tt.requested}, target); got.Eligible {
				t.Fatalf("EvaluateEligibility accepted %q vs %q", tt.requested, tt.gpuName)
			}
		})
	}
}

func TestTargetSpecFromCloudInstancePreservesCloudGPUClassCompatibility(t *testing.T) {
	inst := db.Launch{
		Provider:           string(cloud.ProviderVastai),
		ProviderInstanceID: "vastai:123",
		GPUClass:           "A100",
		ResolvedGPUName:    "A100 SXM4",
		GPUMemGB:           80,
		NumGPUs:            2,
	}
	target := TargetSpecFromCloudInstance(inst)
	c := placement.Constraints{GPUClass: "a100", GPUMemGB: 80, NumGPUs: 2}
	if got := placement.EvaluateEligibility(c, target); !got.Eligible {
		t.Fatalf("EvaluateEligibility = ineligible (%v), want instance accepted", got.Messages())
	}
}

func TestCloudNormalizationDocumentsRTX4080Fork(t *testing.T) {
	requested := "rtx4080"
	gpuName := "RTX 4080S"
	host := inventory.HostSpec{
		Name: "host-4080s",
		GPUs: []inventory.GPUSpec{{
			Name:   gpuName,
			Class:  "rtx4080s",
			Memory: "16GB",
		}},
	}
	if ok, _ := placement.CheckHostGPUConstraints(host, placement.Constraints{GPUClass: requested}); ok {
		t.Fatal("test precondition failed: on-prem structured matcher should reject exact RTX 4080 against RTX 4080S")
	}
	// Reconciliation: Phase 1 keeps structured matching as the predicate, but
	// cloud target normalization absorbs the provider alias so cloud behavior
	// does not regress when callers migrate in Phase 2.
	target := TargetSpecFromOffer(cloud.Offer{
		Provider:   cloud.ProviderVastai,
		ProviderID: "4080s",
		GPUName:    gpuName,
		GPUMemGB:   16,
		NumGPUs:    1,
	})
	if got := placement.EvaluateEligibility(placement.Constraints{GPUClass: requested}, target); !got.Eligible {
		t.Fatalf("EvaluateEligibility = ineligible (%v), want cloud alias accepted", got.Messages())
	}
}

func TestCloudGenerationConstraintUsesStructuredMatcher(t *testing.T) {
	target := TargetSpecFromOffer(cloud.Offer{
		Provider:   cloud.ProviderVastai,
		ProviderID: "4090",
		GPUName:    "RTX 4090",
		GPUMemGB:   24,
		NumGPUs:    1,
	})
	if got := placement.EvaluateEligibility(placement.Constraints{GPUClass: "ampere+"}, target); !got.Eligible {
		t.Fatalf("ampere+ should accept Ada offer after normalization: %v", got.Messages())
	}
	if got := placement.EvaluateEligibility(placement.Constraints{GPUClass: "hopper"}, target); got.Eligible {
		t.Fatal("hopper exact generation should reject Ada offer")
	}
}
