package estimate

import (
	"math"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestGroupKey(t *testing.T) {
	tests := []struct {
		phase PhaseID
		ctx   InstanceContext
		want  string
	}{
		{PhaseStartup, InstanceContext{DataCenter: "US-East"}, "US-East"},
		{PhaseStartup, InstanceContext{}, "_global"},
		{PhaseSSHSetup, InstanceContext{InetDownMbps: 50}, "slow"},
		{PhaseSSHSetup, InstanceContext{InetDownMbps: 200}, "medium"},
		{PhaseSSHSetup, InstanceContext{InetDownMbps: 800}, "fast"},
		{PhaseSSHSetup, InstanceContext{InetDownMbps: 2000}, "very_fast"},
		{PhaseSSHSetup, InstanceContext{InetDownMbps: 0}, "_unknown"},
		{PhaseJobSetup, InstanceContext{CacheWarm: true}, "warm"},
		{PhaseJobSetup, InstanceContext{CacheWarm: false}, "cold:none"},
		{PhaseJobSetup, InstanceContext{CacheWarm: false, DownloadedBytes: 500 * 1024 * 1024}, "cold:small"},
		{PhaseJobSetup, InstanceContext{CacheWarm: false, DownloadedBytes: 5 * 1024 * 1024 * 1024}, "cold:medium"},
		{PhaseJobSetup, InstanceContext{CacheWarm: false, DownloadedBytes: 20 * 1024 * 1024 * 1024}, "cold:large"},
		{PhaseJobSetup, InstanceContext{CacheWarm: false, DownloadedBytes: 60 * 1024 * 1024 * 1024}, "cold:xlarge"},
		{PhaseUpload, InstanceContext{InetUpMbps: 300}, "medium"},
	}

	for _, tt := range tests {
		got := GroupKey(tt.phase, tt.ctx)
		if got != tt.want {
			t.Errorf("GroupKey(%s, %+v) = %q, want %q", tt.phase, tt.ctx, got, tt.want)
		}
	}
}

func TestEstimatePhase_NilModel(t *testing.T) {
	fallback := FromSeconds(45, 30, 60)
	var model *OverheadModel

	est := model.EstimatePhase(PhaseStartup, "any", fallback)
	if est.Mean != fallback.Mean {
		t.Errorf("nil model should return fallback, got %v", est.Mean)
	}
}

func TestEstimatePhase_NoPhaseData(t *testing.T) {
	fallback := FromSeconds(45, 30, 60)
	model := &OverheadModel{Models: map[PhaseID]*HierarchicalModel{}}

	est := model.EstimatePhase(PhaseStartup, "any", fallback)
	if est.Mean != fallback.Mean {
		t.Errorf("empty model should return fallback, got %v", est.Mean)
	}
}

func TestEstimateStartupWithModel(t *testing.T) {
	// With nil model, should return hardcoded default
	est := EstimateStartupWithModel("vastai", nil, InstanceContext{})
	hardcoded := EstimateStartup("vastai")
	if est.Mean != hardcoded.Mean {
		t.Errorf("nil model: got %v, want %v", est.Mean, hardcoded.Mean)
	}
}

func TestEstimateSSHSetup_Default(t *testing.T) {
	est := EstimateSSHSetup(nil, InstanceContext{})
	if est.Mean != FromSeconds(10, 5, 30).Mean {
		t.Errorf("nil model SSH setup mean = %v, want 10s", est.Mean)
	}
}

func TestEstimateJobSetup_Default(t *testing.T) {
	est := EstimateJobSetup(nil, InstanceContext{})
	if est.Mean != FromSeconds(30, 10, 120).Mean {
		t.Errorf("nil model job setup mean = %v, want 30s", est.Mean)
	}
}

func TestEstimateUpload_Default(t *testing.T) {
	est := EstimateUpload(nil, InstanceContext{})
	if est.Mean != FromSeconds(15, 5, 60).Mean {
		t.Errorf("nil model upload mean = %v, want 15s", est.Mean)
	}
}

func TestBuildOverheadModel_Empty(t *testing.T) {
	model := BuildOverheadModel(nil)
	if model != nil {
		t.Error("nil observations should return nil model")
	}

	model = BuildOverheadModel([]db.OverheadObservation{})
	if model != nil {
		t.Error("empty observations should return nil model")
	}
}

func TestBuildOverheadModel_WithData(t *testing.T) {
	obs := make([]db.OverheadObservation, 10)
	for i := range obs {
		startup := 40.0 + float64(i)
		ssh := 8.0 + float64(i)*0.5
		setup := 25.0 + float64(i)*2
		upload := 10.0 + float64(i)
		obs[i] = db.OverheadObservation{
			Provider:     "vastai",
			GPUClass:     "A100",
			DataCenter:   "US-East",
			InetDownMbps: 500,
			InetUpMbps:   200,
			StartupSec:   &startup,
			SSHSetupSec:  &ssh,
			JobSetupSec:  &setup,
			UploadSec:    &upload,
		}
	}

	model := BuildOverheadModel(obs)
	if model == nil {
		t.Fatal("model should not be nil with valid data")
	}

	// Should have models for all 4 phases
	for _, phase := range []PhaseID{PhaseStartup, PhaseSSHSetup, PhaseJobSetup, PhaseUpload} {
		if _, ok := model.Models[phase]; !ok {
			t.Errorf("missing model for phase %s", phase)
		}
	}

	// Startup estimate should be reasonable (~45s)
	ctx := InstanceContext{DataCenter: "US-East", InetDownMbps: 500, InetUpMbps: 200}
	startupEst := EstimateStartupWithModel("vastai", model, ctx)
	if startupEst.Mean < 30*time.Second || startupEst.Mean > 60*time.Second {
		t.Errorf("startup mean = %v, want ~45s", startupEst.Mean)
	}
}

func TestBuildOverheadModel_OutlierFiltering(t *testing.T) {
	// One normal observation + one extreme outlier
	normal := 45.0
	outlier := 100000.0 // way beyond 15-min cap

	obs := []db.OverheadObservation{
		{StartupSec: &normal, DataCenter: "A"},
		{StartupSec: &outlier, DataCenter: "A"},
	}

	model := BuildOverheadModel(obs)
	if model == nil {
		t.Fatal("model should not be nil")
	}

	// The startup model should only have 1 observation (outlier filtered)
	hm := model.Models[PhaseStartup]
	if hm == nil {
		t.Fatal("startup model should exist")
	}

	totalObs := 0
	for _, gs := range hm.Groups {
		totalObs += gs.N
	}
	if totalObs != 1 {
		t.Errorf("expected 1 observation after filtering, got %d", totalObs)
	}
}

func TestBuildOverheadModel_NegativeDurationsFiltered(t *testing.T) {
	negative := -5.0
	zero := 0.0

	obs := []db.OverheadObservation{
		{StartupSec: &negative, DataCenter: "A"},
		{StartupSec: &zero, DataCenter: "A"},
	}

	model := BuildOverheadModel(obs)
	// Both should be filtered (<=0), so no model for startup
	if model != nil {
		if hm, ok := model.Models[PhaseStartup]; ok && hm != nil {
			t.Error("negative/zero durations should be filtered")
		}
	}
}

func TestBuildOverheadModel_CacheWarmColdGrouping(t *testing.T) {
	coldSetup := 60.0
	warmSetup := 10.0
	var coldCache int64 // 0 = cold
	warmCache := int64(5e9)

	obs := []db.OverheadObservation{
		{JobSetupSec: &coldSetup, CacheHFBytes: &coldCache},
		{JobSetupSec: &warmSetup, CacheHFBytes: &warmCache},
	}

	model := BuildOverheadModel(obs)
	if model == nil {
		t.Fatal("model should not be nil")
	}

	hm := model.Models[PhaseJobSetup]
	if hm == nil {
		t.Fatal("job setup model should exist")
	}

	// Should have both cold and warm groups.
	// Cold observations with no download data get key "cold:none".
	if _, ok := hm.Groups["cold:none"]; !ok {
		t.Error("missing 'cold:none' group")
	}
	if _, ok := hm.Groups["warm"]; !ok {
		t.Error("missing 'warm' group")
	}
}

func TestEndToEnd_ModelImprovesFallback(t *testing.T) {
	// Build model with startup times around 60s
	obs := make([]db.OverheadObservation, 20)
	for i := range obs {
		startup := 55.0 + float64(i)*0.5
		obs[i] = db.OverheadObservation{
			StartupSec: &startup,
			DataCenter: "US-West",
		}
	}

	model := BuildOverheadModel(obs)
	ctx := InstanceContext{DataCenter: "US-West"}

	withModel := EstimateStartupWithModel("vastai", model, ctx)
	withoutModel := EstimateStartup("vastai") // hardcoded 45s

	// Model estimate should be closer to 60s than hardcoded 45s
	modelDist := math.Abs(float64(withModel.Mean) - 60*float64(time.Second))
	hardcodedDist := math.Abs(float64(withoutModel.Mean) - 60*float64(time.Second))

	if modelDist >= hardcodedDist {
		t.Errorf("model estimate %v should be closer to 60s than hardcoded %v", withModel.Mean, withoutModel.Mean)
	}
}
