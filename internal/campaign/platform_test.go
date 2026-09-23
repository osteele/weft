package campaign

import (
	"errors"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestPlatformSeparatesGroupsAndFiltersRentalOffers(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, Command: "echo linux", CLIResourceOverrides: &db.CLIResourceOverrides{Platform: "linux/amd64"}},
		{ID: 2, Status: db.StatusQueued, Command: "echo darwin", CLIResourceOverrides: &db.CLIResourceOverrides{Platform: "darwin/arm64"}},
	}
	groups := MergeCompatibleGroups(GroupByAffinity(jobs, nil))
	if len(groups) != 2 {
		t.Fatalf("incompatible execution platforms share a rental group: %+v", groups)
	}
	offers := []cloud.Offer{{Provider: cloud.ProviderVastai, GPUName: "RTX 3090", GPUMemGB: 24, NumGPUs: 1}}
	for _, group := range groups {
		stats := OfferFilterStats{RawCount: len(offers)}
		allowed, ok := applyEligibilityFilters(group, offers, &stats, nil, 0)
		wantAllowed := group.Jobs[0].RequestedPlatform() == "linux/amd64"
		if ok != wantAllowed || (len(allowed) == 1) != wantAllowed {
			t.Fatalf("platform %s: offers=%+v stats=%+v", group.Jobs[0].RequestedPlatform(), allowed, stats)
		}
		if !wantAllowed && stats.PlatformFiltered != 1 {
			t.Fatalf("platform rejection lost its diagnostic: %+v", stats)
		}
	}
}

func TestPlatformConstrainsRentalReuseAndLaunchAdmission(t *testing.T) {
	for _, required := range []string{"linux/amd64", "darwin/arm64", "linux/arm64"} {
		t.Run(required, func(t *testing.T) {
			job := &db.Job{Command: "echo platform", CLIResourceOverrides: &db.CLIResourceOverrides{Platform: required}}
			cap := InstanceCapacity{Instance: &db.Launch{Provider: string(cloud.ProviderVastai)}}
			ok, reason := MatchJobToInstance(job, cap)
			if ok != (required == "linux/amd64") {
				t.Fatalf("reuse %s: allowed=%v reason=%s", required, ok, reason)
			}
			// Exercise final admission even when a caller constructed a group
			// directly without copying the requirement into group metadata.
			_, err := LaunchInstance(nil, nil, nil, nil, InstanceGroup{Jobs: []*db.Job{job}},
				cloud.Offer{Provider: cloud.ProviderVastai}, LaunchOpts{}, cloud.R2Config{}, cloud.CreateOpts{}, R2Assets{}, nil, nil, nil)
			if required == "linux/amd64" {
				if !errors.Is(err, ErrR2ClientRequired) {
					t.Fatalf("matching platform did not reach asset admission: %v", err)
				}
			} else if err == nil || errors.Is(err, ErrR2ClientRequired) {
				t.Fatalf("mismatched platform reached asset/allocation admission: %v", err)
			}
		})
	}
	job := &db.Job{CLIResourceOverrides: &db.CLIResourceOverrides{Platform: "linux/amd64"}}
	if ok, _ := MatchJobToInstance(job, InstanceCapacity{Instance: &db.Launch{Provider: "unknown-provider"}}); ok {
		t.Fatal("unknown rental platform satisfied a declared requirement")
	}
}
