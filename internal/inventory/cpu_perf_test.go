package inventory

import (
	"testing"

	"github.com/osteele/weft/internal/hostinfo"
)

func TestLookupCPUFactor_KnownModels(t *testing.T) {
	tests := []struct {
		model  string
		factor float64
	}{
		{"Apple M2 Max", 2.5},
		{"AMD EPYC 7402 24-Core Processor", 1.0},
		{"Intel(R) Xeon(R) Silver 4210R CPU @ 2.40GHz", 0.8},
		{"Apple M1", 2.2},
		{"Apple M4 Max", 3.6},
	}
	for _, tt := range tests {
		factor, known := LookupCPUFactor(tt.model)
		if !known {
			t.Errorf("LookupCPUFactor(%q): expected known=true", tt.model)
		}
		if factor != tt.factor {
			t.Errorf("LookupCPUFactor(%q) = %.1f, want %.1f", tt.model, factor, tt.factor)
		}
	}
}

func TestLookupCPUFactor_UnknownModel(t *testing.T) {
	factor, known := LookupCPUFactor("Some Future CPU XYZ")
	if known {
		t.Error("expected known=false for unknown CPU")
	}
	if factor != 1.0 {
		t.Errorf("expected factor=1.0 for unknown CPU, got %.1f", factor)
	}
}

func TestLookupCPUFactor_CaseInsensitive(t *testing.T) {
	factor, known := LookupCPUFactor("apple m2 max")
	if !known || factor != 2.5 {
		t.Errorf("case-insensitive match failed: known=%v, factor=%.1f", known, factor)
	}
}

func TestLookupCPUFactor_MostSpecificWins(t *testing.T) {
	// "Apple M2 Max" should match before "Apple M2"
	factor, _ := LookupCPUFactor("Apple M2 Max (38 cores)")
	if factor != 2.5 {
		t.Errorf("expected M2 Max factor 2.5, got %.1f (matched less specific entry?)", factor)
	}
}

func TestHostSpecFromHostInfo_SetsCPUFactor(t *testing.T) {
	info := &hostinfo.Host{
		CPUs:     40,
		CPUModel: "Intel(R) Xeon(R) Silver 4210R CPU @ 2.40GHz",
		Arch:     "Linux x86_64",
		MemTotal: "754Gi",
	}
	spec := HostSpecFromHostInfo("testhost", info, "")
	if spec.CPUFactor != 0.8 {
		t.Errorf("CPUFactor = %.1f, want 0.8 for Xeon Silver 4210R", spec.CPUFactor)
	}
}

func TestHostSpecFromHostInfo_UnknownCPU_DefaultsFactor(t *testing.T) {
	info := &hostinfo.Host{
		CPUs:     8,
		CPUModel: "Some Unknown CPU",
		Arch:     "Linux x86_64",
		MemTotal: "16G",
	}
	spec := HostSpecFromHostInfo("testhost", info, "")
	if spec.CPUFactor != 1.0 {
		t.Errorf("CPUFactor = %.1f, want 1.0 for unknown CPU", spec.CPUFactor)
	}
}
