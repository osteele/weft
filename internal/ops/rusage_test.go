package ops

import (
	"testing"
)

func TestParseResourceUsage(t *testing.T) {
	tests := []struct {
		name           string
		content        string
		wantNil        bool
		wantUserCPU    *float64
		wantSysCPU     *float64
		wantPeakRSS    *int64
		wantMaxGPUMem  *int64
		wantGPUDevices string
	}{
		{
			name:    "empty content",
			content: "",
			wantNil: true,
		},
		{
			name:    "whitespace only",
			content: "   \n  \n",
			wantNil: true,
		},
		{
			name:           "all fields",
			content:        "user_cpu_secs=123.45\nsys_cpu_secs=67.89\npeak_rss_kb=1048576\nmax_gpu_mem_mib=8192\ngpu_devices=0,1\n",
			wantUserCPU:    floatPtr(123.45),
			wantSysCPU:     floatPtr(67.89),
			wantPeakRSS:    int64Ptr(1048576),
			wantMaxGPUMem:  int64Ptr(8192),
			wantGPUDevices: "0,1",
		},
		{
			name:        "cpu only",
			content:     "user_cpu_secs=10.5\nsys_cpu_secs=2.3\n",
			wantUserCPU: floatPtr(10.5),
			wantSysCPU:  floatPtr(2.3),
		},
		{
			name:        "memory only",
			content:     "peak_rss_kb=524288\n",
			wantPeakRSS: int64Ptr(524288),
		},
		{
			name:          "gpu only",
			content:       "max_gpu_mem_mib=16384\n",
			wantMaxGPUMem: int64Ptr(16384),
		},
		{
			name:           "gpu devices only",
			content:        "gpu_devices=0\n",
			wantGPUDevices: "0",
		},
		{
			name:           "gpu devices multiple",
			content:        "gpu_devices=0,1\n",
			wantGPUDevices: "0,1",
		},
		{
			name:    "invalid values ignored",
			content: "user_cpu_secs=not_a_number\npeak_rss_kb=abc\n",
			wantNil: true,
		},
		{
			name:       "mixed valid and invalid",
			content:    "user_cpu_secs=not_a_number\nsys_cpu_secs=5.0\n",
			wantSysCPU: floatPtr(5.0),
		},
		{
			name:    "empty values ignored",
			content: "user_cpu_secs=\nsys_cpu_secs=\n",
			wantNil: true,
		},
		{
			name:    "unknown keys ignored",
			content: "unknown_key=42\nanother=hello\n",
			wantNil: true,
		},
		{
			name:          "extra whitespace",
			content:       "  user_cpu_secs = 1.5 \n  peak_rss_kb = 100 \n  max_gpu_mem_mib = 200 \n",
			wantUserCPU:   floatPtr(1.5),
			wantPeakRSS:   int64Ptr(100),
			wantMaxGPUMem: int64Ptr(200),
		},
		{
			name:        "zero values are valid",
			content:     "user_cpu_secs=0.00\nsys_cpu_secs=0.00\npeak_rss_kb=0\n",
			wantUserCPU: floatPtr(0.0),
			wantSysCPU:  floatPtr(0.0),
			wantPeakRSS: int64Ptr(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseResourceUsage(tt.content)
			if tt.wantNil {
				if result != nil {
					t.Errorf("expected nil, got %+v", result)
				}
				return
			}
			if result == nil {
				t.Fatal("expected non-nil result")
			}

			checkFloat64Ptr(t, "UserCPUSecs", result.UserCPUSecs, tt.wantUserCPU)
			checkFloat64Ptr(t, "SysCPUSecs", result.SysCPUSecs, tt.wantSysCPU)
			checkInt64Ptr(t, "PeakRSSKB", result.PeakRSSKB, tt.wantPeakRSS)
			checkInt64Ptr(t, "MaxGPUMemMiB", result.MaxGPUMemMiB, tt.wantMaxGPUMem)
			if result.GPUDevices != tt.wantGPUDevices {
				t.Errorf("GPUDevices: expected %q, got %q", tt.wantGPUDevices, result.GPUDevices)
			}
		})
	}
}

func checkFloat64Ptr(t *testing.T, name string, got, want *float64) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s: expected nil, got %f", name, *got)
		}
		return
	}
	if got == nil {
		t.Errorf("%s: expected %f, got nil", name, *want)
		return
	}
	if *got != *want {
		t.Errorf("%s: expected %f, got %f", name, *want, *got)
	}
}

func checkInt64Ptr(t *testing.T, name string, got, want *int64) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("%s: expected nil, got %d", name, *got)
		}
		return
	}
	if got == nil {
		t.Errorf("%s: expected %d, got nil", name, *want)
		return
	}
	if *got != *want {
		t.Errorf("%s: expected %d, got %d", name, *want, *got)
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}
