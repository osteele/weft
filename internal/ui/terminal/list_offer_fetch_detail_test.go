package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
)

func TestRenderGroupedStatusOfferFetchUnavailableShowsExpandedCause(t *testing.T) {
	now := time.Unix(5_000, 0)
	reason := "planner: provider offer fetch unavailable: provider command timed out; market unknown; Weft will retry"
	job := &db.Job{
		ID:                 2030,
		Status:             db.StatusQueued,
		Project:            "proj",
		Description:        "waiting",
		QueuedAt:           4_000,
		QueueBlockedReason: reason,
	}
	detail := map[int64]*blockreason.Structured{
		job.ID: {
			Summary:      reason,
			Launch:       reason,
			LaunchDetail: "provider command timed out",
		},
	}
	out := groupedRowsText(buildGroupedStatusRowsWithOptions([]*db.Job{job}, 0, groupedStatusRenderOptions{
		now:             now,
		blockedDetail:   detail,
		expandedBlocked: map[int64]bool{job.ID: true},
	}))

	if !strings.Contains(out, "waiting: planner: provider offer fetch unavailable") {
		t.Fatalf("missing compact waiting reason:\n%s", out)
	}
	if !strings.Contains(out, "new instance  "+reason) || !strings.Contains(out, "provider command timed out") {
		t.Fatalf("missing expanded provider cause:\n%s", out)
	}
}
