package cmd

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestParseLaunchOpts(t *testing.T) {
	tests := []struct {
		name           string
		maxSpend       string
		maxTime        string
		wantSpendCents int
		wantTimeSecs   int
	}{
		{"empty", "", "", 0, 0},
		{"spend with dollar sign", "$5.00", "", 500, 0},
		{"spend without dollar sign", "10.50", "", 1050, 0},
		{"time 2h", "", "2h", 0, 7200},
		{"time 30m", "", "30m", 0, 1800},
		{"both", "$2.50", "1h", 250, 3600},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set globals (restored after test)
			oldSpend, oldTime := campaignLaunchMaxSpend, campaignLaunchMaxTime
			defer func() {
				campaignLaunchMaxSpend = oldSpend
				campaignLaunchMaxTime = oldTime
			}()

			campaignLaunchMaxSpend = tt.maxSpend
			campaignLaunchMaxTime = tt.maxTime

			opts := parseLaunchOpts()

			if opts.MaxSpendCents != tt.wantSpendCents {
				t.Errorf("MaxSpendCents = %d, want %d", opts.MaxSpendCents, tt.wantSpendCents)
			}
			if opts.MaxTimeSeconds != tt.wantTimeSecs {
				t.Errorf("MaxTimeSeconds = %d, want %d", opts.MaxTimeSeconds, tt.wantTimeSecs)
			}
		})
	}
}

func intPtr(n int) *int { return &n }

func TestFilterJobsByIDs(t *testing.T) {
	jobs := []*db.Job{
		{ID: 10, Status: db.StatusQueued},
		{ID: 20, Status: db.StatusQueued},
		{ID: 30, Status: db.StatusQueued},
	}

	// Filter to jobs 10 and 30
	filter := map[int64]bool{10: true, 30: true}
	var filtered []*db.Job
	for _, j := range jobs {
		if filter[j.ID] {
			filtered = append(filtered, j)
		}
	}

	if len(filtered) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(filtered))
	}
	if filtered[0].ID != 10 || filtered[1].ID != 30 {
		t.Errorf("expected jobs [10, 30], got [%d, %d]", filtered[0].ID, filtered[1].ID)
	}
}

func TestNonInteractiveLaunchGrouping(t *testing.T) {
	// Verify that jobs are grouped correctly for non-interactive launch
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued, GPUClass: "H100", GPUMemGB: intPtr(80)},
		{ID: 2, Status: db.StatusQueued, GPUClass: "H100", GPUMemGB: intPtr(80)},
		{ID: 3, Status: db.StatusQueued, GPUClass: "A100", GPUMemGB: intPtr(40)},
	}

	groups := campaign.GroupByGPUSupremum(jobs)
	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}

	// H100 group should have 2 jobs
	h100Jobs := 0
	a100Jobs := 0
	for _, g := range groups {
		switch g.GPUClass {
		case "H100":
			h100Jobs = len(g.Jobs)
		case "A100":
			a100Jobs = len(g.Jobs)
		}
	}
	if h100Jobs != 2 {
		t.Errorf("H100 group: expected 2 jobs, got %d", h100Jobs)
	}
	if a100Jobs != 1 {
		t.Errorf("A100 group: expected 1 job, got %d", a100Jobs)
	}
}

func TestParseLaunchOptsTimeDuration(t *testing.T) {
	oldTime := campaignLaunchMaxTime
	defer func() { campaignLaunchMaxTime = oldTime }()

	campaignLaunchMaxTime = "1h30m"
	opts := parseLaunchOpts()

	expected := int((1*time.Hour + 30*time.Minute).Seconds())
	if opts.MaxTimeSeconds != expected {
		t.Errorf("MaxTimeSeconds = %d, want %d", opts.MaxTimeSeconds, expected)
	}
}
