package cmd

import (
	"strings"
	"testing"
)

func TestInterconnectClassPhrase(t *testing.T) {
	for name, want := range map[string]string{
		"A100 SXM4 80GB": "NVSwitch class; every pair NVLink",
		"H100 SXM":       "NVSwitch class; every pair NVLink",
		"H100 NVL":       "NVLink-bridged pairs",
		"H200 NVL":       "NVLink-bridged pairs",
		"A100 PCIE 40GB": "no NVLink indicated by naming",
		"B200":           "no NVLink indicated by naming",
	} {
		if got := interconnectClassPhrase(name); got != want {
			t.Errorf("interconnectClassPhrase(%q) = %q, want %q", name, got, want)
		}
	}
}

// wj6307 is the case this warning exists for: nvlink requested on 4 GPUs,
// served by a PCIe part whose provider-reported bandwidth was real but came
// from bridges between pairs. Nothing in the job's own output contradicted the
// request, so the fabric was only discovered by probing from inside the job.
func TestInterconnectTopologyWarning(t *testing.T) {
	warning := interconnectTopologyWarning("nvlink", "A100 PCIE 40GB", 4)
	if warning == "" {
		t.Fatal("4 GPUs on a non-NVSwitch part must warn")
	}
	for _, want := range []string{"not comparable", "nvlink-uniform"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning %q missing %q", warning, want)
		}
	}

	for _, tc := range []struct {
		name      string
		requested string
		delivered string
		numGPUs   int
	}{
		{"NVSwitch part is a single domain", "nvlink", "A100 SXM4 80GB", 8},
		{"a pair needs no switch", "nvlink", "A100 PCIE 40GB", 2},
		{"uniform was requested, so placement already enforced it", "nvlink-uniform", "A100 SXM4", 8},
		{"no interconnect requested", "", "A100 PCIE 40GB", 4},
		{"delivered hardware unknown", "nvlink", "", 4},
	} {
		if got := interconnectTopologyWarning(tc.requested, tc.delivered, tc.numGPUs); got != "" {
			t.Errorf("%s: unexpected warning %q", tc.name, got)
		}
	}
}
