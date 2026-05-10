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
			name:      "a5000 retry matches RTX A5000",
			gpuClass:  "a5000",
			wantNames: []string{"RTX A5000"},
		},
		{
			name:      "rtxa5000 exact",
			gpuClass:  "rtxa5000",
			wantNames: []string{"RTX A5000"},
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
			name:          "a5000+ promotes to ampere+ via retry",
			gpuClass:      "a5000+",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "RTX 4090",
			rejectGPUName: "RTX 2080 Ti",
		},
		{
			name:      "short alias 3090",
			gpuClass:  "3090",
			wantNames: []string{"RTX 3090"},
		},
		{
			name:      "short alias 3080",
			gpuClass:  "3080",
			wantNames: []string{"RTX 3080"},
		},
		{
			name:      "rtx-4080 alias includes 4080 and 4080S",
			gpuClass:  "rtx-4080",
			wantNames: []string{"RTX 4080", "RTX 4080S"},
		},
		{
			name:      "short alias 2080ti",
			gpuClass:  "2080ti",
			wantNames: []string{"RTX 2080 Ti"},
		},
		{
			name:      "v100 exact",
			gpuClass:  "v100",
			wantNames: []string{"Tesla V100"},
		},
		{
			name:          "volta generation constraint",
			gpuClass:      "volta",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "Tesla V100",
			rejectGPUName: "RTX 2080 Ti",
		},
		{
			name:          "volta+ includes newer generations",
			gpuClass:      "volta+",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "RTX 5090",
			rejectGPUName: "",
		},
		{
			name:     "nvidia family",
			gpuClass: "nvidia",
		},
		{
			name:      "a100sxm variant",
			gpuClass:  "A100 SXM",
			wantNames: []string{"A100 SXM4"},
		},
		{
			name:      "a100pcie variant",
			gpuClass:  "A100 PCIE",
			wantNames: []string{"A100 PCIE"},
		},
		{
			name:      "h100nvl variant",
			gpuClass:  "H100 NVL",
			wantNames: []string{"H100 NVL"},
		},
		{
			name:      "h100sxm variant",
			gpuClass:  "H100 SXM",
			wantNames: []string{"H100 SXM"},
		},
		{
			name:      "prefix a100sxm4",
			gpuClass:  "A100 SXM4",
			wantNames: []string{"A100 SXM4"},
		},
		{
			name:      "a100 sxm4 memory suffix variant",
			gpuClass:  "A100-SXM4-80GB",
			wantNames: []string{"A100 SXM4"},
		},
		{
			name:      "prefix b200nvl",
			gpuClass:  "B200 NVL",
			wantNames: []string{"B200 NVL"},
		},
		{
			name:      "prefix h200nvl",
			gpuClass:  "H200 NVL",
			wantNames: []string{"H200 NVL"},
		},
		{
			name:          "prefix a100sxm+ min-mode",
			gpuClass:      "A100 SXM+",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "RTX 4090",
			rejectGPUName: "RTX 2080 Ti",
		},
		{
			name:          "ampere+ accepts RTX 3090 Ti",
			gpuClass:      "ampere+",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "RTX 3090 Ti",
			rejectGPUName: "RTX 2080 Ti",
		},
		{
			name:          "ampere+ accepts RTX 5090",
			gpuClass:      "ampere+",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "RTX 5090",
			rejectGPUName: "RTX 2080",
		},
		{
			name:          "ada accepts RTX 4080S",
			gpuClass:      "ada",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "RTX 4080S",
			rejectGPUName: "RTX 3090",
		},
		{
			name:          "ada accepts RTX 6000Ada",
			gpuClass:      "ada",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "RTX 6000Ada",
			rejectGPUName: "RTX 5090",
		},
		{
			name:          "blackwell accepts RTX 5090",
			gpuClass:      "blackwell",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "RTX 5090",
			rejectGPUName: "RTX 4090",
		},
		{
			name:          "blackwell accepts RTX PRO 5000",
			gpuClass:      "blackwell",
			wantNames:     nil,
			wantPostFilt:  true,
			acceptGPUName: "RTX PRO 5000",
			rejectGPUName: "RTX PRO 6000 WS",
		},
		{
			name:      "exact model RTX 3090 Ti",
			gpuClass:  "3090ti",
			wantNames: []string{"RTX 3090 Ti"},
		},
		{
			name:      "exact model RTX 4080S",
			gpuClass:  "RTX 4080S",
			wantNames: []string{"RTX 4080S"},
		},
		{
			name:      "unknown gpu passthrough",
			gpuClass:  "xyz999",
			wantNames: []string{"xyz999"},
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

func TestGPUClassMatchesOfferName(t *testing.T) {
	tests := []struct {
		name     string
		gpuClass string
		gpuName  string
		want     bool
	}{
		{name: "empty class matches all", gpuClass: "", gpuName: "RTX 4090", want: true},
		{name: "rtx-4080 matches 4080s", gpuClass: "rtx-4080", gpuName: "RTX 4080S", want: true},
		{name: "a6000 matches RTX A6000", gpuClass: "a6000", gpuName: "RTX A6000", want: true},
		{name: "rtx-5090 matches RTX 5090", gpuClass: "rtx-5090", gpuName: "RTX 5090", want: true},
		{name: "v100 matches Tesla V100", gpuClass: "v100", gpuName: "Tesla V100", want: true},
		{name: "volta matches Tesla V100", gpuClass: "volta", gpuName: "Tesla V100", want: true},
		{name: "volta rejects turing", gpuClass: "volta", gpuName: "RTX 2080 Ti", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := GPUClassMatchesOfferName(tt.gpuClass, tt.gpuName); got != tt.want {
				t.Fatalf("GPUClassMatchesOfferName(%q, %q) = %v, want %v", tt.gpuClass, tt.gpuName, got, tt.want)
			}
		})
	}
}
