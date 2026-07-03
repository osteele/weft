package main

import (
	"testing"

	"github.com/osteele/weft/internal/cloud"
)

func TestParseGPUCount(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want int
	}{
		{name: "two GPUs", out: "0\n1\n", want: 2},
		{name: "single GPU", out: "0\n", want: 1},
		{name: "whitespace and blank lines", out: "\n 0 \n\n 1 \n", want: 2},
		{name: "empty output means zero GPUs", out: "", want: 0},
		{name: "NVML error is unparseable", out: "Failed to initialize NVML: Unknown Error\n", want: -1},
		{name: "driver mismatch banner is unparseable", out: "NVIDIA-SMI has failed\n", want: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseGPUCount(tc.out); got != tc.want {
				t.Fatalf("parseGPUCount(%q) = %d, want %d", tc.out, got, tc.want)
			}
		})
	}
}

func TestMaxRequestedGPUCount(t *testing.T) {
	cases := []struct {
		name string
		jobs []cloud.AgentJob
		want int
	}{
		{name: "no jobs", jobs: nil, want: 0},
		{name: "single-GPU jobs", jobs: []cloud.AgentJob{{GPUCount: 0}, {GPUCount: 1}}, want: 1},
		{
			name: "multi-GPU job dominates",
			jobs: []cloud.AgentJob{{GPUCount: 1}, {GPUCount: 2}, {GPUCount: 0}},
			want: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := maxRequestedGPUCount(tc.jobs); got != tc.want {
				t.Fatalf("maxRequestedGPUCount = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCheckGPUCountSkipsSingleGPURequests(t *testing.T) {
	// requiredCount <= 1 must be a no-op regardless of nvidia-smi state —
	// the probe only guards multi-GPU shapes (wb41), matching the runner's
	// gpu_count_preflight threshold.
	for _, required := range []int{0, 1} {
		if terminated := checkGPUCount("", 1, required, ""); terminated {
			t.Fatalf("checkGPUCount(required=%d) terminated, want no-op", required)
		}
	}
}
