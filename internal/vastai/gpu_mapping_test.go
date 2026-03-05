package vastai

import (
	"testing"
)

func TestResolveGPUFilter(t *testing.T) {
	tests := []struct {
		name          string
		gpuClass      string
		wantNames     []string // expected vastaiNames (nil = post-filter mode)
		wantPostFilt  bool     // whether postFilter should be non-nil
		acceptGPUName string   // a gpu_name that should pass post-filter
		rejectGPUName string   // a gpu_name that should fail post-filter
	}{
		{
			name:      "empty",
			gpuClass:  "",
			wantNames: nil,
		},
		{
			name:      "exact model a100",
			gpuClass:  "a100",
			wantNames: []string{"A100 PCIE", "A100 SXM4", "A100X"},
		},
		{
			name:      "exact model RTX 4090",
			gpuClass:  "4090",
			wantNames: []string{"RTX 4090"},
		},
		{
			name:      "case insensitive A100",
			gpuClass:  "A100",
			wantNames: []string{"A100 PCIE", "A100 SXM4", "A100X"},
		},
		{
			name:      "l40s exact",
			gpuClass:  "l40s",
			wantNames: []string{"L40S"},
		},
		{
			name:          "hopper+ generation constraint",
			gpuClass:      "hopper+",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "H100 SXM",
			rejectGPUName: "RTX 4090",
		},
		{
			name:          "hopper exact generation",
			gpuClass:      "hopper",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "H100 SXM",
			rejectGPUName: "B200",
		},
		{
			name:          "a100+ promotes to ampere+",
			gpuClass:      "a100+",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "RTX 4090",
			rejectGPUName: "RTX 2080 Ti",
		},
		{
			name:     "nvidia family",
			gpuClass: "nvidia",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names, postFilter := resolveGPUFilter(tt.gpuClass)

			if tt.wantNames != nil {
				if len(names) != len(tt.wantNames) {
					t.Fatalf("got %d names %v, want %d %v", len(names), names, len(tt.wantNames), tt.wantNames)
				}
				for i, want := range tt.wantNames {
					if names[i] != want {
						t.Errorf("name[%d] = %q, want %q", i, names[i], want)
					}
				}
			}

			if tt.wantPostFilt && postFilter == nil {
				t.Fatal("expected postFilter, got nil")
			}

			if postFilter != nil && tt.acceptGPUName != "" {
				offers := []Offer{
					{GPUName: tt.acceptGPUName},
					{GPUName: tt.rejectGPUName},
				}
				filtered := postFilter(offers)
				if len(filtered) != 1 || filtered[0].GPUName != tt.acceptGPUName {
					t.Errorf("postFilter: got %v, want [%s]", filtered, tt.acceptGPUName)
				}
			}
		})
	}
}
