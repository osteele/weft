package campaign

import "testing"

func TestFilterByGPUClassUsesConstraintCompatibility(t *testing.T) {
	groups := []InstanceGroup{
		{GPUClass: "A100 PCIE"},
		{GPUClass: "A100 SXM4"},
		{GPUClass: "H100 PCIE"},
		{GPUClass: ""},
	}

	tests := []struct {
		name   string
		filter string
		want   []string
	}{
		{"empty returns all", "", []string{"A100 PCIE", "A100 SXM4", "H100 PCIE", ""}},
		{"broad a100 includes variants", "A100", []string{"A100 PCIE", "A100 SXM4"}},
		{"narrow pcie remains narrow", "A100 PCIE", []string{"A100 PCIE"}},
		{"narrow sxm includes sxm4", "A100 SXM", []string{"A100 SXM4"}},
		{"broad h100 includes variant", "H100", []string{"H100 PCIE"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterByGPUClass(groups, tt.filter)
			if len(got) != len(tt.want) {
				t.Fatalf("FilterByGPUClass(%q) returned %d groups, want %d: %+v", tt.filter, len(got), len(tt.want), got)
			}
			for i := range tt.want {
				if got[i].GPUClass != tt.want[i] {
					t.Fatalf("FilterByGPUClass(%q)[%d].GPUClass = %q, want %q", tt.filter, i, got[i].GPUClass, tt.want[i])
				}
			}
		})
	}
}
