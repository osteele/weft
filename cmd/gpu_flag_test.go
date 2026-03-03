package cmd

import "testing"

func TestParseGPUFlag(t *testing.T) {
	tests := []struct {
		input     string
		wantClass string
		wantMem   int
		wantErr   bool
	}{
		// Plain values (no memory)
		{"nvidia", "nvidia", 0, false},
		{"ampere+", "ampere+", 0, false},
		{"a100", "a100", 0, false},

		// Combined syntax
		{"nvidia>=24", "nvidia", 24, false},
		{"nvidia>=24GB", "nvidia", 24, false},
		{"nvidia>=24gb", "nvidia", 24, false},
		{"ampere+>=40GB", "ampere+", 40, false},
		{"a100>=80GB", "a100", 80, false},

		// Error cases
		{"nvidia>=0", "", 0, true},
		{"nvidia>=abc", "", 0, true},
		{"nvidia>=", "", 0, true},
		{">=24GB", "", 0, true},
	}

	for _, tt := range tests {
		gpuClass, gpuMem, err := parseGPUFlag(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("parseGPUFlag(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if tt.wantErr {
			continue
		}
		if gpuClass != tt.wantClass {
			t.Errorf("parseGPUFlag(%q) class = %q, want %q", tt.input, gpuClass, tt.wantClass)
		}
		if gpuMem != tt.wantMem {
			t.Errorf("parseGPUFlag(%q) mem = %d, want %d", tt.input, gpuMem, tt.wantMem)
		}
	}
}
