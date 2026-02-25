package cmd

import "testing"

func TestResolveGPUMemGB(t *testing.T) {
	tests := []struct {
		name       string
		gpuMemFlag int
		gpu        string
		gpuClass   string
		wantNil    bool
		wantVal    int
	}{
		{
			name:       "no GPU, no flag",
			gpuMemFlag: 0,
			gpu:        "",
			wantNil:    true,
		},
		{
			name:       "GPU present, no flag — uses default",
			gpuMemFlag: 0,
			gpu:        "0",
			wantVal:    defaultGPUMemGB,
		},
		{
			name:       "GPU present, explicit flag",
			gpuMemFlag: 10,
			gpu:        "0",
			wantVal:    10,
		},
		{
			name:       "no GPU, explicit flag — uses flag anyway",
			gpuMemFlag: 10,
			gpu:        "",
			wantVal:    10,
		},
		{
			name:       "multiple GPUs, no flag",
			gpuMemFlag: 0,
			gpu:        "0,1",
			wantVal:    defaultGPUMemGB,
		},
		{
			name:       "multiple GPUs, explicit flag",
			gpuMemFlag: 40,
			gpu:        "0,1",
			wantVal:    40,
		},
		{
			name:     "GPU class, no flag — uses default",
			gpuClass: "A100",
			wantVal:  defaultGPUMemGB,
		},
		{
			name:       "GPU class with explicit flag",
			gpuClass:   "A100",
			gpuMemFlag: 40,
			wantVal:    40,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolveGPUMemGB(tt.gpuMemFlag, tt.gpu, tt.gpuClass)
			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil, got %d", *result)
				}
				return
			}
			if result == nil {
				t.Errorf("expected %d, got nil", tt.wantVal)
				return
			}
			if *result != tt.wantVal {
				t.Errorf("expected %d, got %d", tt.wantVal, *result)
			}
		})
	}
}

func TestIsNumericGPU(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"0", true},
		{"0,1", true},
		{"2,3,4", true},
		{"A100", false},
		{"RTX 3090", false},
		{"2080", true}, // pure digits, treated as device index
		{"", false},
		{"a", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isNumericGPU(tt.input)
			if got != tt.want {
				t.Errorf("isNumericGPU(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
