package campaign

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestEstimateGroupDisk_RejectsAnomalousHistoryAndSurfacesIt(t *testing.T) {
	database := db.SetupTestDB(t)
	bogusBytes := int64(231_107_044_835_328) // ~231 TB, real wj2301 sample
	addHistoricalDiskRecord(t, database, "llm-performance-models",
		"bash scripts/collect_all.sh --skip-vllm", bogusBytes)
	addHistoricalDiskRecord(t, database, "llm-performance-models",
		"uv run python scripts/calibrate_power.py --duration 10 --sweep", bogusBytes)

	disk, anomalies := EstimateGroupDisk(llmPerfModelsTestGroup(), database, nil)
	if disk != DefaultMinDiskGB {
		t.Fatalf("disk = %d, want %d (estimator should reject bogus history "+
			"and fall back to input-based / floor)", disk, DefaultMinDiskGB)
	}
	if len(anomalies) == 0 {
		t.Fatal("expected anomalies to be reported, got none")
	}
	var foundJob1, foundJob2 bool
	for _, a := range anomalies {
		if a.Source != "phase_timings" {
			continue
		}
		if a.ObservedBytes != bogusBytes {
			t.Errorf("ObservedBytes = %d, want %d", a.ObservedBytes, bogusBytes)
		}
		if a.BoundBytes != DiskTelemetryPlausibilityBytes {
			t.Errorf("BoundBytes = %d, want %d", a.BoundBytes, DiskTelemetryPlausibilityBytes)
		}
		if strings.Contains(a.Command, "collect_all") {
			foundJob1 = true
		}
		if strings.Contains(a.Command, "calibrate_power") {
			foundJob2 = true
		}
	}
	if !foundJob1 || !foundJob2 {
		t.Errorf("expected anomalies for both jobs, found collect_all=%v calibrate_power=%v",
			foundJob1, foundJob2)
	}
}

func TestEstimateGroupDisk_AcceptsPlausibleHistory(t *testing.T) {
	database := db.SetupTestDB(t)
	// 50 GB is well within the plausibility bound.
	plausible := int64(50_000_000_000)
	addHistoricalDiskRecord(t, database, "llm-performance-models",
		"bash scripts/collect_all.sh --skip-vllm", plausible)
	addHistoricalDiskRecord(t, database, "llm-performance-models",
		"uv run python scripts/calibrate_power.py --duration 10 --sweep", plausible)

	_, anomalies := EstimateGroupDisk(llmPerfModelsTestGroup(), database, nil)
	if len(anomalies) != 0 {
		t.Fatalf("expected no anomalies for plausible history, got %d", len(anomalies))
	}
}

func TestFormatDiskAnomalySummary_DedupesAndOrders(t *testing.T) {
	anomalies := []DiskTelemetryAnomaly{
		{JobID: 2301, Source: "phase_timings", ObservedBytes: 100_000_000_000_000},
		{JobID: 2301, Source: "timeseries", ObservedBytes: 231_107_044_835_328},
		{JobID: 2337, Source: "timeseries", ObservedBytes: 230_749_391_290_368},
	}
	summary := formatDiskAnomalySummary(anomalies)
	if !strings.Contains(summary, "wj2301") {
		t.Errorf("summary missing wj2301: %q", summary)
	}
	if !strings.Contains(summary, "wj2337") {
		t.Errorf("summary missing wj2337: %q", summary)
	}
	// The larger observed for wj2301 (timeseries, 231.1TB) should win over phase_timings (100.0TB).
	if !strings.Contains(summary, "231.1TB") {
		t.Errorf("summary did not pick the larger observed for wj2301: %q", summary)
	}
	if !strings.Contains(summary, "2.0TB plausibility bound") {
		t.Errorf("summary missing plausibility bound reference: %q", summary)
	}
	if !strings.Contains(summary, "weft job anomalies") {
		t.Errorf("summary missing follow-up command pointer: %q", summary)
	}
}

func TestFormatDiskAnomalySummary_Empty(t *testing.T) {
	if got := formatDiskAnomalySummary(nil); got != "" {
		t.Errorf("formatDiskAnomalySummary(nil) = %q, want empty", got)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0B"},
		{999, "999B"},
		{1_000, "1.0KB"},
		{1_500_000, "1.5MB"},
		{50_000_000_000, "50.0GB"},
		{2_000_000_000_000, "2.0TB"},
		{231_107_044_835_328, "231.1TB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
