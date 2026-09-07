package opsqueue

import "testing"

func TestMissingRunnerCapabilityBlockDetailRoundTrip(t *testing.T) {
	detail := MissingRunnerCapabilityBlockDetail(
		CapabilityArtifactNeedV1,
		"agent update required before dispatch",
	)
	if !IsMissingRunnerCapabilityBlock(detail) {
		t.Fatalf("IsMissingRunnerCapabilityBlock(%q) = false, want true", detail)
	}
}

func TestIsMissingRunnerCapabilityBlock(t *testing.T) {
	tests := []struct {
		name   string
		detail string
		want   bool
	}{
		{
			name:   "queue update remedy",
			detail: "queue runner lacks job-payload-v1 capability; run `weft queue update studio` to deploy a current agent",
			want:   true,
		},
		{
			name:   "agent update remedy",
			detail: "queue runner lacks artifact-need-v1 capability; agent update required before dispatch",
			want:   true,
		},
		{
			name:   "queue append stage wrapper",
			detail: "queue append failed: queue runner lacks artifact-need-v1 capability; agent update required before dispatch",
			want:   true,
		},
		{
			name:   "unrelated dispatch block",
			detail: "source sync deferred (host unreachable): ssh timeout",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsMissingRunnerCapabilityBlock(tt.detail); got != tt.want {
				t.Fatalf("IsMissingRunnerCapabilityBlock(%q) = %v, want %v", tt.detail, got, tt.want)
			}
		})
	}
}
