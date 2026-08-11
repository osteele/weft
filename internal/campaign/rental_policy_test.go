package campaign

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestGroupByAffinitySeparatesRentalPolicies(t *testing.T) {
	capA := 100
	capB := 200
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "A100", CLIResourceOverrides: &db.CLIResourceOverrides{MaxHourlyRateCents: &capA}},
		{ID: 2, Status: db.StatusQueued, GPUClass: "A100", CLIResourceOverrides: &db.CLIResourceOverrides{MaxHourlyRateCents: &capB}},
	}
	groups := GroupByAffinity(jobs, nil)
	if len(groups) != 2 {
		t.Fatalf("groups = %d, want 2 for different rental policies", len(groups))
	}
}

func TestApplyJobRentalPolicyUsesJobCaps(t *testing.T) {
	maxSpend := 8300
	maxTime := 9000
	grace := 0
	minSurvival := 0.9
	job := &db.Job{CLIResourceOverrides: &db.CLIResourceOverrides{
		MaxSpendCents:      &maxSpend,
		MaxTimeSeconds:     &maxTime,
		GracePeriodSeconds: &grace,
		MinSurvival:        &minSurvival,
	}}

	got, err := applyJobRentalPolicy(LaunchOpts{GracePeriodSeconds: 15 * 60, MinSurvival: 0.4}, []*db.Job{job})
	if err != nil {
		t.Fatalf("applyJobRentalPolicy: %v", err)
	}
	if got.MaxSpendCents != maxSpend || got.MaxTimeSeconds != maxTime || got.GracePeriodSeconds != 0 || got.MinSurvival != minSurvival {
		t.Fatalf("launch opts = %+v", got)
	}
}

func TestApplyJobRentalPolicyAllowsExplicitZeroSurvival(t *testing.T) {
	minSurvival := 0.0
	job := &db.Job{CLIResourceOverrides: &db.CLIResourceOverrides{MinSurvival: &minSurvival}}

	got, err := applyJobRentalPolicy(LaunchOpts{MinSurvival: 0.4}, []*db.Job{job})
	if err != nil {
		t.Fatal(err)
	}
	if got.MinSurvival != 0 {
		t.Fatalf("MinSurvival = %v, want explicit disabled value 0", got.MinSurvival)
	}
}

func TestValidateOfferHourlyRateCap(t *testing.T) {
	cap := 3320
	group := InstanceGroup{Jobs: []*db.Job{{CLIResourceOverrides: &db.CLIResourceOverrides{MaxHourlyRateCents: &cap}}}}
	if err := validateOfferHourlyRateCap(group, cloud.Offer{CostPerHour: 33.20}); err != nil {
		t.Fatalf("offer at cap rejected: %v", err)
	}
	err := validateOfferHourlyRateCap(group, cloud.Offer{CostPerHour: 33.21})
	if err == nil || !strings.Contains(err.Error(), "exceeds job max-hourly-rate") {
		t.Fatalf("error = %v, want hourly-rate rejection", err)
	}
}
