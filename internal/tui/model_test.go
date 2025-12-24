package tui

import (
	"testing"

	"github.com/osteele/remote-jobs/internal/db"
)

func TestGetTargetJobPrefersHighlightedInDetailsTab(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Host: "host-a"},
		{ID: 2, Host: "host-b"},
	}

	m := Model{
		jobs:          jobs,
		selectedIndex: 0,
		selectedJob:   jobs[1],
		detailTab:     DetailTabDetails,
	}

	if got := m.getTargetJob(); got == nil || got.ID != 1 {
		t.Fatalf("expected highlighted job 1 in Details tab, got %+v", got)
	}

	m.detailTab = DetailTabLogs
	if got := m.getTargetJob(); got == nil || got.ID != 2 {
		t.Fatalf("expected selected log job 2 in Logs tab, got %+v", got)
	}
}
