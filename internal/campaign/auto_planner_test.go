package campaign

import (
	"errors"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestApplyGroupOffer_LaunchableSkipsOnlyReusedJobs(t *testing.T) {
	plan := AutoPlacementPlan{BlockedReasons: map[int64]string{}}
	group := InstanceGroup{
		Jobs: []*db.Job{
			{ID: 706},
			{ID: 707},
		},
	}
	offer := GroupOffer{Offer: &cloud.Offer{}}
	reused := map[int64]struct{}{706: {}}

	applyGroupOffer(&plan, group, offer, reused)

	if len(plan.LaunchJobIDs) != 1 || plan.LaunchJobIDs[0] != 707 {
		t.Fatalf("launch job ids = %v, want [707]", plan.LaunchJobIDs)
	}
	if len(plan.BlockedReasons) != 0 {
		t.Fatalf("blocked reasons = %v, want none", plan.BlockedReasons)
	}
}

func TestApplyGroupOffer_BlocksOnlyUnreusedJobsWhenNoOffer(t *testing.T) {
	plan := AutoPlacementPlan{BlockedReasons: map[int64]string{}}
	group := InstanceGroup{
		Jobs: []*db.Job{
			{ID: 706},
			{ID: 707},
		},
	}
	offer := GroupOffer{Err: errors.New("capacity unavailable")}
	reused := map[int64]struct{}{706: {}}

	applyGroupOffer(&plan, group, offer, reused)

	if len(plan.LaunchJobIDs) != 0 {
		t.Fatalf("launch job ids = %v, want []", plan.LaunchJobIDs)
	}
	if got := plan.BlockedReasons[707]; got != "planner: capacity unavailable" {
		t.Fatalf("blocked reason for 707 = %q, want %q", got, "planner: capacity unavailable")
	}
	if _, exists := plan.BlockedReasons[706]; exists {
		t.Fatalf("unexpected blocked reason for reused job 706: %q", plan.BlockedReasons[706])
	}
}
