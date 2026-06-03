package campaign

import (
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func insertHistoricalLaunch(t *testing.T, database *sql.DB, gpuClass string, gpuMemGB, costCents int, launchedAt time.Time) {
	t.Helper()
	_, err := database.Exec(
		`INSERT INTO launches (campaign_id, status, provider, gpu_class, gpu_mem_gb, cost_per_hour_cents, created_at, launched_at)
		 VALUES (NULL, 'completed', 'vastai', ?, ?, ?, ?, ?)`,
		gpuClass, gpuMemGB, costCents, launchedAt.Unix(), launchedAt.Unix(),
	)
	if err != nil {
		t.Fatalf("insert launch: %v", err)
	}
}

func TestPriceAnchor_ReturnsNilWhenHistoryEmpty(t *testing.T) {
	database := db.SetupTestDB(t)
	anchor, err := PriceAnchorForClass(database, "A100", 80)
	if err != nil {
		t.Fatalf("PriceAnchorForClass: %v", err)
	}
	if anchor != nil {
		t.Fatalf("expected nil anchor when no history exists, got %+v", anchor)
	}
}

func TestPriceAnchor_RequiresMinSamples(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now()
	// Only two samples; below PriceAnchorMinSamples=3 in every window.
	insertHistoricalLaunch(t, database, "A100", 80, 100, now.AddDate(0, 0, -1))
	insertHistoricalLaunch(t, database, "A100", 80, 120, now.AddDate(0, 0, -2))

	anchor, err := PriceAnchorForClass(database, "A100", 80)
	if err != nil {
		t.Fatalf("PriceAnchorForClass: %v", err)
	}
	if anchor != nil {
		t.Fatalf("expected nil anchor with %d samples (< minimum %d), got %+v",
			2, PriceAnchorMinSamples, anchor)
	}
}

func TestPriceAnchor_PicksSmallestSatisfyingWindow(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now()
	// Three recent samples in the 30d window — anchor should use that window.
	insertHistoricalLaunch(t, database, "A100", 80, 100, now.AddDate(0, 0, -5))
	insertHistoricalLaunch(t, database, "A100", 80, 110, now.AddDate(0, 0, -10))
	insertHistoricalLaunch(t, database, "A100", 80, 120, now.AddDate(0, 0, -15))
	// Plus older noise the window should ignore.
	insertHistoricalLaunch(t, database, "A100", 80, 999, now.AddDate(0, 0, -200))

	anchor, err := PriceAnchorForClass(database, "A100", 80)
	if err != nil {
		t.Fatalf("PriceAnchorForClass: %v", err)
	}
	if anchor == nil {
		t.Fatalf("expected anchor, got nil")
	}
	if anchor.WindowDays != 30 {
		t.Errorf("WindowDays = %d, want 30 (smallest satisfying window)", anchor.WindowDays)
	}
	if anchor.SampleCount != 3 {
		t.Errorf("SampleCount = %d, want 3 (old sample excluded by window)", anchor.SampleCount)
	}
}

func TestPriceAnchor_BacksOffForSparseClasses(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now()
	// All samples are 60+ days old → 30d window has 0, 90d has 3.
	insertHistoricalLaunch(t, database, "H200", 141, 200, now.AddDate(0, 0, -60))
	insertHistoricalLaunch(t, database, "H200", 141, 220, now.AddDate(0, 0, -70))
	insertHistoricalLaunch(t, database, "H200", 141, 240, now.AddDate(0, 0, -80))

	anchor, err := PriceAnchorForClass(database, "H200", 141)
	if err != nil {
		t.Fatalf("PriceAnchorForClass: %v", err)
	}
	if anchor == nil {
		t.Fatalf("expected anchor from 90d window backoff, got nil")
	}
	if anchor.WindowDays != 90 {
		t.Errorf("WindowDays = %d, want 90 (backed off from 30d)", anchor.WindowDays)
	}
}

func TestPriceAnchor_ExcludesUnlaunchedRows(t *testing.T) {
	database := db.SetupTestDB(t)
	now := time.Now()
	insertHistoricalLaunch(t, database, "A100", 80, 100, now.AddDate(0, 0, -1))
	insertHistoricalLaunch(t, database, "A100", 80, 110, now.AddDate(0, 0, -2))
	insertHistoricalLaunch(t, database, "A100", 80, 120, now.AddDate(0, 0, -3))
	// A launch that never came up (planned, no launched_at) — must not enter
	// the anchor; the user never paid that price.
	if _, err := database.Exec(
		`INSERT INTO launches (campaign_id, status, provider, gpu_class, gpu_mem_gb, cost_per_hour_cents, created_at)
		 VALUES (NULL, 'planned', 'runpod', 'A100', 80, 9999, ?)`,
		now.Unix(),
	); err != nil {
		t.Fatalf("insert planned launch: %v", err)
	}

	anchor, err := PriceAnchorForClass(database, "A100", 80)
	if err != nil {
		t.Fatalf("PriceAnchorForClass: %v", err)
	}
	if anchor == nil {
		t.Fatalf("expected anchor, got nil")
	}
	if anchor.MaxCents == 9999 {
		t.Errorf("anchor includes never-launched row (max=%d)", anchor.MaxCents)
	}
}

func TestPriceAnchor_AutoApproveCeiling(t *testing.T) {
	a := &PriceAnchor{P75Cents: 100}
	if got, want := a.AutoApproveCeilingCents(), int(100*PriceAnchorAutoApproveMultiplier); got != want {
		t.Errorf("AutoApproveCeilingCents = %d, want %d", got, want)
	}
	if got := (*PriceAnchor)(nil).AutoApproveCeilingCents(); got != 0 {
		t.Errorf("nil anchor AutoApproveCeilingCents = %d, want 0", got)
	}
}

func TestPercentileInt_NearestRank(t *testing.T) {
	cases := []struct {
		sorted []int
		p      float64
		want   int
	}{
		{[]int{10, 20, 30, 40}, 0.25, 10},
		{[]int{10, 20, 30, 40}, 0.50, 20},
		{[]int{10, 20, 30, 40}, 0.75, 30},
		{[]int{10, 20, 30, 40}, 1.00, 40},
		{[]int{10, 20, 30, 40}, 0.0, 10},
		{[]int{5}, 0.5, 5},
		{[]int{}, 0.5, 0},
	}
	for _, c := range cases {
		if got := percentileInt(c.sorted, c.p); got != c.want {
			t.Errorf("percentileInt(%v, %.2f) = %d, want %d", c.sorted, c.p, got, c.want)
		}
	}
}
