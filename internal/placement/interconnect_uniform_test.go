package placement

import "testing"

func f64(v float64) *float64 { return &v }

// wj6307 asked for nvlink on 4 GPUs and was served a host of two NVLink pairs
// bridged by PCIe, whose collective timings are not comparable with a
// single-domain host. "nvlink" is a presence test and correctly still accepts
// that host; "nvlink-uniform" is the constraint that rejects it.
func TestInterconnectUniformRejectsBridgedPairsAboveTwoGPUs(t *testing.T) {
	cases := []struct {
		name        string
		signals     string
		bandwidth   *float64
		numGPUs     int
		wantLoose   bool
		wantUniform bool
	}{
		{
			name: "wj6307: PCIe part reporting bridge bandwidth, 4 GPUs",
			// The provider measurement is positive because the bridges are
			// real; it says nothing about whether all four reach each other.
			signals: "A100 PCIE 40GB", bandwidth: f64(112), numGPUs: 4,
			wantLoose: true, wantUniform: false,
		},
		{
			name:    "same host at 2 GPUs is uniform by construction",
			signals: "A100 PCIE 40GB", bandwidth: f64(112), numGPUs: 2,
			wantLoose: true, wantUniform: true,
		},
		{
			name:    "NVL is a bridged pair, so 4 GPUs are islands",
			signals: "H100 NVL", bandwidth: nil, numGPUs: 4,
			wantLoose: true, wantUniform: false,
		},
		{
			name:    "SXM sits on an NVSwitch baseboard",
			signals: "A100 SXM4 80GB", bandwidth: nil, numGPUs: 8,
			wantLoose: true, wantUniform: true,
		},
		{
			name:    "SXM with a measurement is still uniform",
			signals: "H100 SXM", bandwidth: f64(900), numGPUs: 8,
			wantLoose: true, wantUniform: true,
		},
		{
			name: "bare model name is not evidence of topology",
			// B200 parts are usually NVSwitch-based, but the name does not say
			// so; an unverifiable topology must fail closed rather than be
			// assumed.
			signals: "B200", bandwidth: f64(900), numGPUs: 8,
			wantLoose: true, wantUniform: false,
		},
		{
			name:    "plain PCIe with no NVLink fails both",
			signals: "A100 PCIE 40GB", bandwidth: f64(0), numGPUs: 4,
			wantLoose: false, wantUniform: false,
		},
		{
			name:    "single GPU needs no fabric",
			signals: "A100 PCIE 40GB", bandwidth: f64(112), numGPUs: 1,
			wantLoose: true, wantUniform: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InterconnectSatisfied(InterconnectNVLink, []string{tc.signals}, tc.bandwidth, tc.numGPUs); got != tc.wantLoose {
				t.Errorf("nvlink = %v, want %v", got, tc.wantLoose)
			}
			if got := InterconnectSatisfied(InterconnectNVLinkUniform, []string{tc.signals}, tc.bandwidth, tc.numGPUs); got != tc.wantUniform {
				t.Errorf("nvlink-uniform = %v, want %v", got, tc.wantUniform)
			}
		})
	}
}

// The looser values must not shift when a stricter one is added beside them.
func TestInterconnectLooseValuesUnchangedByGPUCount(t *testing.T) {
	for _, n := range []int{0, 1, 2, 4, 8} {
		if !InterconnectSatisfied("", []string{"A100 PCIE"}, nil, n) {
			t.Errorf("empty requirement rejected at %d GPUs", n)
		}
		if !InterconnectSatisfied(InterconnectAny, []string{"A100 PCIE"}, nil, n) {
			t.Errorf("any rejected at %d GPUs", n)
		}
		if !InterconnectSatisfied(InterconnectPCIe, []string{"A100 PCIE"}, f64(0), n) {
			t.Errorf("pcie rejected a PCIe host at %d GPUs", n)
		}
		if InterconnectSatisfied(InterconnectPCIe, []string{"A100 SXM4"}, nil, n) {
			t.Errorf("pcie accepted an SXM host at %d GPUs", n)
		}
	}
}

// A host holding both SXM and PCIe cards must not let the SXM naming vouch for
// GPUs a job cannot use. Before per-device evaluation the whole host was one
// concatenated string, so "sxm" anywhere satisfied uniformity for every GPU on
// the box.
func TestInterconnectUniformJudgesEachDeviceNotTheWholeHost(t *testing.T) {
	mixed := []string{"A100 SXM4 80GB a100", "RTX 2080 Ti rtx2080ti"}
	if InterconnectSatisfied(InterconnectNVLinkUniform, mixed, nil, 4) {
		t.Error("a mixed SXM/PCIe candidate set must not satisfy nvlink-uniform")
	}
	if !InterconnectSatisfied(InterconnectNVLinkUniform, []string{"A100 SXM4 80GB a100", "A100 SXM4 80GB a100"}, nil, 4) {
		t.Error("an all-SXM candidate set must satisfy nvlink-uniform")
	}
	// pcie is the converse and was wrong in the same way: an SXM card
	// elsewhere on the host made a PCIe-pinned job look non-PCIe.
	if !InterconnectSatisfied(InterconnectPCIe, []string{"RTX 2080 Ti rtx2080ti"}, nil, 2) {
		t.Error("a PCIe-only candidate set must satisfy pcie")
	}
	// An empty candidate set cannot demonstrate uniformity and must not be
	// read as vacuously true.
	if InterconnectSatisfied(InterconnectNVLinkUniform, nil, nil, 4) {
		t.Error("no candidate devices must not satisfy nvlink-uniform")
	}
}
