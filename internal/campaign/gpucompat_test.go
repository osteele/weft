package campaign

import "testing"

func TestGPUClassSupremum(t *testing.T) {
	tests := []struct {
		a, b       string
		wantMerged string
		wantOK     bool
	}{
		// Empty is unconstrained
		{"", "", "", true},
		{"", "nvidia", "NVIDIA", true},
		{"nvidia", "", "NVIDIA", true},
		{"", "ampere", "AMPERE", true},
		{"ampere", "", "AMPERE", true},
		{"", "a100", "A100", true},

		// Same class
		{"nvidia", "nvidia", "NVIDIA", true},
		{"ampere", "ampere", "AMPERE", true},
		{"a100", "a100", "A100", true},
		{"h100-pcie", "h100-pcie", "H100-PCIE", true},

		// Family subsumes generation and model
		{"nvidia", "ampere", "AMPERE", true},
		{"nvidia", "a100", "A100", true},
		{"nvidia", "hopper+", "HOPPER+", true},

		// Generation subsumes model within same gen
		{"ampere", "a100", "A100", true},
		{"ampere", "rtx3090", "RTX3090", true},

		// MinGen subsumes higher minGen and gens/models in range
		{"ampere+", "hopper+", "HOPPER+", true},
		{"ampere+", "hopper", "HOPPER", true},
		{"ampere+", "h100", "H100", true},
		{"hopper+", "h100-hbm3", "H100-HBM3", true},

		// Incompatible: different generations
		{"ampere", "turing", "", false},
		{"ampere", "hopper", "", false},
		{"turing", "hopper", "", false},

		// Incompatible: different exact models (even same gen)
		{"a100", "rtx3090", "", false},
		{"h100-pcie", "h100-sxm", "", false},
		{"h100-pcie", "h100-hbm3", "", false},
		{"h100-nvl", "h100-hbm3", "", false},

		// Broad model selectors merge with compatible variants.
		{"h100", "h100-pcie", "H100-PCIE", true},
		{"h100", "h100-sxm", "H100-SXM", true},
		{"h100", "h100-hbm3", "H100-HBM3", true},
		{"h100-sxm", "h100-hbm3", "H100-HBM3", true},

		// Incompatible: cross-family
		{"nvidia", "apple", "", false},
		{"ampere", "m2max", "", false},

		// Case normalization (always returns uppercase)
		{"NVIDIA", "Ampere", "AMPERE", true},
	}

	for _, tt := range tests {
		merged, ok := gpuClassSupremum(tt.a, tt.b)
		if ok != tt.wantOK {
			t.Errorf("gpuClassSupremum(%q, %q) ok = %v, want %v", tt.a, tt.b, ok, tt.wantOK)
			continue
		}
		if ok && merged != tt.wantMerged {
			t.Errorf("gpuClassSupremum(%q, %q) = %q, want %q", tt.a, tt.b, merged, tt.wantMerged)
		}
	}
}
