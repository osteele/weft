package cmd

import "testing"

func TestResolveGPUMemGB(t *testing.T) {
	tests := []struct {
		name       string
		gpuMemFlag int
		gpu        string
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolveGPUMemGB(tt.gpuMemFlag, tt.gpu)
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
