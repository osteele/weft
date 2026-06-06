package placement

import (
	"strings"
	"testing"
)

func TestAssessHostLoadClassifiesLiveMetrics(t *testing.T) {
	tests := []struct {
		name    string
		metrics *HostMetrics
		want    HostLoadState
		reason  string
	}{
		{
			name:    "normal",
			metrics: &HostMetrics{GPUPercent: 40, CPUPercent: 30, RAMPercent: 50},
			want:    HostLoadNormal,
		},
		{
			name:    "busy",
			metrics: &HostMetrics{GPUPercent: 88, CPUPercent: 30, RAMPercent: 50},
			want:    HostLoadBusy,
			reason:  "gpu 88%",
		},
		{
			name:    "overloaded",
			metrics: &HostMetrics{GPUPercent: 99, CPUPercent: 30, RAMPercent: 50},
			want:    HostLoadOverloaded,
			reason:  "gpu 99%",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := AssessHostLoad(nil, "host-alpha", tc.metrics, DefaultHostLoadOptions())
			if got.State != tc.want {
				t.Fatalf("State = %s, want %s", got.State, tc.want)
			}
			if tc.reason != "" && !strings.Contains(strings.ToLower(got.Reason), tc.reason) {
				t.Fatalf("Reason = %q, want substring %q", got.Reason, tc.reason)
			}
		})
	}
}

func TestAssessHostsLoadIncludesOnlyNonNormalHosts(t *testing.T) {
	got := AssessHostsLoad(nil, map[string]*HostMetrics{
		"normal":     {GPUPercent: 10},
		"overloaded": {RAMPercent: 97},
	}, DefaultHostLoadOptions())
	if _, ok := got["normal"]; ok {
		t.Fatal("normal host should not be included")
	}
	if got["overloaded"].State != HostLoadOverloaded {
		t.Fatalf("overloaded state = %s, want %s", got["overloaded"].State, HostLoadOverloaded)
	}
}
