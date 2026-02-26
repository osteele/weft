package placement

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := dataloc.InitSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestScoreHosts_NoConstraints(t *testing.T) {
	db := setupTestDB(t)
	scores, err := ScoreHosts(db, Constraints{})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) < 3 {
		t.Fatalf("expected at least 3 hosts, got %d", len(scores))
	}
	for _, s := range scores {
		if !s.Eligible {
			t.Errorf("host %s should be eligible with no constraints", s.Host)
		}
	}
}

func TestScoreHosts_GPUClass_A100(t *testing.T) {
	db := setupTestDB(t)
	scores, err := ScoreHosts(db, Constraints{GPUClass: "a100"})
	if err != nil {
		t.Fatal(err)
	}

	// cool100 should be eligible (has A100s), others should not
	for _, s := range scores {
		switch s.Host {
		case "cool100":
			if !s.Eligible {
				t.Errorf("cool100 should be eligible for A100")
			}
		case "cool30":
			if s.Eligible {
				t.Errorf("cool30 should not be eligible for A100")
			}
		case "studio":
			if s.Eligible {
				t.Errorf("studio should not be eligible for A100")
			}
		}
	}
}

func TestScoreHosts_GPUClass_RTX3090(t *testing.T) {
	db := setupTestDB(t)
	scores, err := ScoreHosts(db, Constraints{GPUClass: "rtx3090"})
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range scores {
		if s.Host == "cool30" && !s.Eligible {
			t.Error("cool30 should be eligible for rtx3090")
		}
		if s.Host == "cool100" && s.Eligible {
			t.Error("cool100 should not be eligible for rtx3090")
		}
	}
}

func TestScoreHosts_GPUMemory(t *testing.T) {
	db := setupTestDB(t)
	scores, err := ScoreHosts(db, Constraints{GPUMemGB: 48})
	if err != nil {
		t.Fatal(err)
	}

	for _, s := range scores {
		switch s.Host {
		case "cool100":
			if !s.Eligible {
				t.Error("cool100 should be eligible (A100 has 80GB)")
			}
		case "cool30":
			if s.Eligible {
				t.Error("cool30 should not be eligible (RTX 3090 has 24GB)")
			}
		case "studio":
			if !s.Eligible {
				t.Error("studio should be eligible (M2 Max has 96GB)")
			}
		}
	}
}

func TestScoreHosts_DataLocality(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now()

	// Place a model on cool30
	if err := dataloc.RecordAsset(db, dataloc.HostDataEntry{
		Host:     "cool30",
		Asset:    dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "meta-llama/Llama-3-8B"},
		LastSeen: now,
	}); err != nil {
		t.Fatal(err)
	}

	scores, err := ScoreHosts(db, Constraints{
		Inputs: []string{"hf:meta-llama/Llama-3-8B"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// cool30 should rank first due to data locality
	if scores[0].Host != "cool30" {
		t.Errorf("expected cool30 first (has local data), got %s", scores[0].Host)
	}
}

func TestScoreHosts_DataLocality_MultipleInputs(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now()

	// cool100 has both assets
	for _, id := range []string{"meta-llama/Llama-3-8B", "wikitext"} {
		kind := dataloc.AssetHFModel
		if id == "wikitext" {
			kind = dataloc.AssetHFDataset
		}
		if err := dataloc.RecordAsset(db, dataloc.HostDataEntry{
			Host: "cool100", Asset: dataloc.DataAsset{Kind: kind, ID: id}, LastSeen: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// cool30 has only one
	if err := dataloc.RecordAsset(db, dataloc.HostDataEntry{
		Host: "cool30", Asset: dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "meta-llama/Llama-3-8B"}, LastSeen: now,
	}); err != nil {
		t.Fatal(err)
	}

	scores, err := ScoreHosts(db, Constraints{
		Inputs: []string{"hf:meta-llama/Llama-3-8B", "hf-dataset:wikitext"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// cool100 should rank higher (2/2 local vs 1/2)
	cool100Score := findScore(scores, "cool100")
	cool30Score := findScore(scores, "cool30")
	if cool100Score.Total <= cool30Score.Total {
		t.Errorf("cool100 (%.1f) should score higher than cool30 (%.1f)", cool100Score.Total, cool30Score.Total)
	}
}

func TestScoreHosts_CombinedConstraints(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now()

	// Place data on cool30
	if err := dataloc.RecordAsset(db, dataloc.HostDataEntry{
		Host: "cool30", Asset: dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "model"}, LastSeen: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Require A100 (cool100 only) + data locality (cool30 only)
	scores, err := ScoreHosts(db, Constraints{
		GPUClass: "a100",
		Inputs:   []string{"hf:model"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// cool100 should be only eligible host (has A100), even though data is on cool30
	for _, s := range scores {
		if s.Host == "cool100" && !s.Eligible {
			t.Error("cool100 should be eligible (has A100)")
		}
		if s.Host == "cool30" && s.Eligible {
			t.Error("cool30 should be ineligible (no A100)")
		}
	}
}

func TestBestHost_NoConstraints(t *testing.T) {
	db := setupTestDB(t)
	host, _, err := BestHost(db, Constraints{})
	if err != nil {
		t.Fatal(err)
	}
	if host == "" {
		t.Error("expected a host, got empty")
	}
}

func TestBestHost_Impossible(t *testing.T) {
	db := setupTestDB(t)
	_, _, err := BestHost(db, Constraints{GPUClass: "nonexistent"})
	if err == nil {
		t.Error("expected error for impossible constraints")
	}
}

func TestParseMemGB(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"80GB", 80},
		{"24GB", 24},
		{"11GB", 11},
		{"96GB", 96},
		{"0GB", 0},
	}
	for _, tt := range tests {
		got := parseMemGB(tt.input)
		if got != tt.want {
			t.Errorf("parseMemGB(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestSortScores(t *testing.T) {
	scores := []Score{
		{Host: "a", Eligible: false, Total: 100},
		{Host: "b", Eligible: true, Total: 5},
		{Host: "c", Eligible: true, Total: 10},
	}
	sortScores(scores)

	// Eligible first, then by score
	if scores[0].Host != "c" {
		t.Errorf("expected c first (eligible, highest score), got %s", scores[0].Host)
	}
	if scores[1].Host != "b" {
		t.Errorf("expected b second, got %s", scores[1].Host)
	}
	if scores[2].Host != "a" {
		t.Errorf("expected a last (ineligible), got %s", scores[2].Host)
	}
}

func TestTransferCostScoring(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now()

	// Place a large model (15GB) on cool30 only
	if err := dataloc.RecordAsset(db, dataloc.HostDataEntry{
		Host:      "cool30",
		Asset:     dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "big-model/15gb"},
		SizeBytes: 15_000_000_000, // 15GB
		LastSeen:  now,
	}); err != nil {
		t.Fatal(err)
	}

	scores, err := ScoreHosts(db, Constraints{
		Inputs: []string{"hf:big-model/15gb"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// cool30 has the data locally — should score highest
	cool30 := findScore(scores, "cool30")
	cool100 := findScore(scores, "cool100")
	studio := findScore(scores, "studio")

	if !cool30.Eligible || !cool100.Eligible || !studio.Eligible {
		t.Error("all hosts should be eligible with no hard constraints")
	}

	if cool30.Total <= cool100.Total {
		t.Errorf("cool30 (%.2f) should score higher than cool100 (%.2f) — cool30 has data local", cool30.Total, cool100.Total)
	}

	// cool100 has 10Gbps, studio has 1Gbps — cool100 should get less penalty
	if cool100.Total <= studio.Total {
		t.Errorf("cool100 (%.2f) should score higher than studio (%.2f) — cool100 has 10x bandwidth", cool100.Total, studio.Total)
	}

	// Verify reason strings mention transfer
	hasTransferReason := false
	for _, r := range cool100.Reasons {
		if strings.Contains(r, "transfer") {
			hasTransferReason = true
			break
		}
	}
	if !hasTransferReason {
		t.Errorf("cool100 reasons should mention transfer, got: %v", cool100.Reasons)
	}
}

func TestTransferCostScoring_AllLocal(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now()

	// Place data on cool100
	if err := dataloc.RecordAsset(db, dataloc.HostDataEntry{
		Host:      "cool100",
		Asset:     dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "model-a"},
		SizeBytes: 5_000_000_000,
		LastSeen:  now,
	}); err != nil {
		t.Fatal(err)
	}

	scores, err := ScoreHosts(db, Constraints{
		Inputs: []string{"hf:model-a"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// cool100 should have no transfer penalty
	cool100 := findScore(scores, "cool100")
	for _, r := range cool100.Reasons {
		if strings.Contains(r, "transfer") {
			t.Errorf("cool100 should not have transfer reason when data is local, got: %v", cool100.Reasons)
		}
	}
}

func TestUtilizationScoring_PrefersIdleHost(t *testing.T) {
	db := setupTestDB(t)

	metrics := map[string]*HostMetrics{
		"cool30":  {GPUPercent: 90, CPUPercent: 80},
		"cool100": {GPUPercent: 10, CPUPercent: 5},
		"studio":  {GPUPercent: 50, CPUPercent: 40},
	}

	scores, err := ScoreHostsWithMetrics(db, Constraints{}, metrics)
	if err != nil {
		t.Fatal(err)
	}

	cool30 := findScore(scores, "cool30")
	cool100 := findScore(scores, "cool100")

	if cool100.Total <= cool30.Total {
		t.Errorf("cool100 (%.2f, 10%% GPU) should score higher than cool30 (%.2f, 90%% GPU)", cool100.Total, cool30.Total)
	}
}

func TestUtilizationScoring_QueueDepth(t *testing.T) {
	db := setupTestDB(t)

	metrics := map[string]*HostMetrics{
		"cool30":  {QueueDepth: 0},
		"cool100": {QueueDepth: 5},
		"studio":  {QueueDepth: 2},
	}

	scores, err := ScoreHostsWithMetrics(db, Constraints{}, metrics)
	if err != nil {
		t.Fatal(err)
	}

	cool30 := findScore(scores, "cool30")
	cool100 := findScore(scores, "cool100")
	studio := findScore(scores, "studio")

	if cool30.Total <= studio.Total {
		t.Errorf("cool30 (%.2f, 0 queued) should score higher than studio (%.2f, 2 queued)", cool30.Total, studio.Total)
	}
	if studio.Total <= cool100.Total {
		t.Errorf("studio (%.2f, 2 queued) should score higher than cool100 (%.2f, 5 queued)", studio.Total, cool100.Total)
	}

	// Verify reason string
	hasQueueReason := false
	for _, r := range cool100.Reasons {
		if strings.Contains(r, "queued") {
			hasQueueReason = true
			break
		}
	}
	if !hasQueueReason {
		t.Errorf("cool100 reasons should mention queue depth, got: %v", cool100.Reasons)
	}
}

func TestUtilizationScoring_NilMetrics(t *testing.T) {
	db := setupTestDB(t)

	// Nil metrics should work the same as ScoreHosts
	scoresWithNil, err := ScoreHostsWithMetrics(db, Constraints{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	scoresWithout, err := ScoreHosts(db, Constraints{})
	if err != nil {
		t.Fatal(err)
	}

	for i := range scoresWithNil {
		if scoresWithNil[i].Total != scoresWithout[i].Total {
			t.Errorf("host %s: nil metrics score %.2f != no-metrics score %.2f",
				scoresWithNil[i].Host, scoresWithNil[i].Total, scoresWithout[i].Total)
		}
	}
}

func TestUtilizationScoring_CombinedWithConstraints(t *testing.T) {
	db := setupTestDB(t)

	// cool100 is the only host with A100, but it's heavily loaded
	metrics := map[string]*HostMetrics{
		"cool100": {GPUPercent: 95, CPUPercent: 90, QueueDepth: 3},
	}

	scores, err := ScoreHostsWithMetrics(db, Constraints{GPUClass: "a100"}, metrics)
	if err != nil {
		t.Fatal(err)
	}

	// cool100 should still be the only eligible host despite load
	cool100 := findScore(scores, "cool100")
	if !cool100.Eligible {
		t.Error("cool100 should be eligible (only host with A100)")
	}

	// Verify it has utilization reasons
	hasGPUReason := false
	for _, r := range cool100.Reasons {
		if strings.Contains(r, "GPU") && strings.Contains(r, "loaded") {
			hasGPUReason = true
		}
	}
	if !hasGPUReason {
		t.Errorf("cool100 should have GPU load reason, got: %v", cool100.Reasons)
	}
}

func findScore(scores []Score, host string) Score {
	for _, s := range scores {
		if s.Host == host {
			return s
		}
	}
	return Score{}
}
