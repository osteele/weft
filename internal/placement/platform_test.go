package placement

import (
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
)

func TestPlatformEligibilityCPUHosts(t *testing.T) {
	for _, tc := range []struct {
		name, os, arch string
		eligible       bool
	}{
		{"matching uname alias", "Linux", "x86_64", true},
		{"wrong OS and arch", "Darwin", "arm64", false},
		{"wrong arch", "linux", "arm64", false},
		{"wrong OS", "darwin", "amd64", false},
		{"unknown arch", "linux", "", false},
		{"unknown OS", "", "amd64", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := inventory.HostSpec{Name: "host-alpha", OS: tc.os, Arch: tc.arch}
			got := CheckHostConstraints(host, Constraints{Platform: "linux/amd64"})
			if got.Eligible != tc.eligible {
				t.Fatalf("eligible = %v, want %v: %v", got.Eligible, tc.eligible, got.Messages())
			}
			if !got.Eligible && got.Reasons[0].Kind != ReasonPlatform {
				t.Fatalf("wrong rejection axis: %v", got.Reasons)
			}
			if unconstrained := CheckHostConstraints(host, Constraints{}); !unconstrained.Eligible {
				t.Fatalf("missing platform constraint changed CPU eligibility: %v", unconstrained.Messages())
			}
		})
	}
}

func TestPersistedPlatformConstrainsCPUPlacement(t *testing.T) {
	job := &db.Job{Command: "echo numerical-work", CLIResourceOverrides: &db.CLIResourceOverrides{Platform: "linux/amd64"}}
	constraints := ConstraintsFromJob(job)
	if got := EvaluateEligibility(constraints, TargetSpec{Platform: "darwin/arm64"}); got.Eligible {
		t.Fatal("persisted platform was dropped for CPU-only job")
	}
	if got := EvaluateEligibility(constraints, TargetSpec{Platform: "linux/amd64"}); !got.Eligible {
		t.Fatalf("matching CPU platform rejected: %v", got.Messages())
	}
	job.CLIResourceOverrides.Platform = "invalid"
	if got := EvaluateEligibility(ConstraintsFromJob(job), TargetSpec{Platform: "linux/amd64"}); got.Eligible {
		t.Fatal("invalid persisted platform silently relaxed the requirement")
	}
}
