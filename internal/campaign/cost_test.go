package campaign

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
	"github.com/osteele/weft/internal/predictor"
)

func TestEstimateCosts_NoPredictions(t *testing.T) {
	groupOffers := []GroupOffer{
		{
			Group: InstanceGroup{GPUClass: "A100", GPUMemGB: 80, Jobs: []*db.Job{{ID: 1}, {ID: 2}, {ID: 3}}},
			Offer: &cloud.Offer{GPUName: "A100 PCIE", GPUMemGB: 80, CostPerHour: 1.50},
		},
	}

	estimates := EstimateCosts(nil, groupOffers, nil, nil, nil, nil, nil)

	if len(estimates) != 1 {
		t.Fatalf("expected 1 estimate, got %d", len(estimates))
	}

	est := estimates[0]
	if len(est.JobDurations) > 0 {
		t.Error("should not have prediction with nil config")
	}

	// 3 jobs * default duration + startup + provision overhead
	expectedJobTime := 3 * estimate.DefaultJobDuration.Mean
	if est.TotalTime < expectedJobTime {
		t.Errorf("TotalTime = %v, should be >= %v (3 jobs * default)", est.TotalTime, expectedJobTime)
	}

	if est.TotalCost <= 0 {
		t.Error("TotalCost should be > 0")
	}
}

func TestEstimateCosts_SlottedGroupUsesMaxRuntime(t *testing.T) {
	groupOffers := []GroupOffer{
		{
			Group: InstanceGroup{
				GPUClass: "A40",
				NumGPUs:  2,
				SlotGPUs: true,
				GPUMemGB: 48,
				Jobs:     []*db.Job{{ID: 1}, {ID: 2}},
			},
			Offer: &cloud.Offer{GPUName: "A40", NumGPUs: 2, GPUMemGB: 48, CostPerHour: 2.00},
		},
	}

	estimates := EstimateCosts(nil, groupOffers, nil, nil, nil, nil, nil)
	got := estimates[0].Breakdown.Run.Mean
	if got != estimate.DefaultJobDuration.Mean {
		t.Fatalf("slotted Run.Mean = %v, want one default duration %v", got, estimate.DefaultJobDuration.Mean)
	}
}

func TestCompletionTimeEstimate_SlottedJobsAreNotQueued(t *testing.T) {
	est := CostEstimate{
		Group: InstanceGroup{
			SlotGPUs: true,
			Jobs:     []*db.Job{{ID: 1}, {ID: 2}},
		},
		Breakdown: estimate.Breakdown{
			Run:   estimate.Constant(time.Hour),
			Total: estimate.Constant(70 * time.Minute),
		},
		JobDurations: map[int64]time.Duration{
			1: 30 * time.Minute,
			2: time.Hour,
		},
		TotalTime: 70 * time.Minute,
	}

	got := completionTimeEstimate(est, 2)
	want := 110 * time.Minute // 2*(10m shared setup) + 30m + 60m
	if got.Mean != want {
		t.Fatalf("completion time = %v, want %v", got.Mean, want)
	}
}

func TestEstimateCosts_NilOffer(t *testing.T) {
	groupOffers := []GroupOffer{
		{
			Group: InstanceGroup{GPUClass: "H100", Jobs: []*db.Job{{ID: 1}}},
			Offer: nil,
		},
	}

	estimates := EstimateCosts(nil, groupOffers, nil, nil, nil, nil, nil)
	if estimates[0].TotalCost != 0 {
		t.Errorf("nil offer should have 0 cost, got %f", estimates[0].TotalCost)
	}
}

func TestEstimateCostsCountsNamedAssetBytes(t *testing.T) {
	database := db.SetupTestDB(t)
	if err := db.UpsertNamedAsset(database, db.NamedAsset{
		Name:        "trace-v1",
		ContentHash: strings.Repeat("a", 64),
		SizeBytes:   40_000_000_000,
		ContentType: string(dataloc.ContentTypeDirectory),
		TargetPath:  "data/trace-v1",
	}); err != nil {
		t.Fatalf("UpsertNamedAsset: %v", err)
	}
	groupOffers := []GroupOffer{{
		Group: InstanceGroup{Jobs: []*db.Job{{
			ID:     1,
			Inputs: []string{"asset:trace-v1"},
		}}},
		Offer: &cloud.Offer{GPUName: "A100 PCIE", GPUMemGB: 80, CostPerHour: 1.50, DownloadBandwidth: 1000},
	}}

	estimates := EstimateCosts(database, groupOffers, nil, nil, nil, nil, nil)
	if got := estimates[0].DownloadBytes; got < 40_000_000_000 {
		t.Fatalf("DownloadBytes = %d, want named asset bytes counted", got)
	}
}

func TestEstimateReuseGroup_NoPredictionsDoesNotScaleByDLPerf(t *testing.T) {
	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs: []*db.Job{
			{ID: 1, Command: "python train.py --epochs 1"},
			{ID: 2, Command: "python train.py --epochs 2"},
		},
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{
			GPUClass:        "NVIDIA",
			ResolvedGPUName: "H200",
			GPUMemGB:        141,
			DLPerf:          40,
			InetDownMbps:    1000,
			InetUpMbps:      1000,
			Status:          db.LaunchStatusRunning,
		},
	}

	est, ok := EstimateReuseGroup(nil, group, cap, nil, nil)
	if !ok {
		t.Fatal("expected reuse estimate")
	}

	wantRun := 2 * estimate.DefaultJobDuration.Mean
	if est.Breakdown.Run.Mean != wantRun {
		t.Fatalf("Run.Mean = %v, want %v", est.Breakdown.Run.Mean, wantRun)
	}
	if est.JobDurations[1] != estimate.DefaultJobDuration.Mean {
		t.Fatalf("job 1 duration = %v, want %v", est.JobDurations[1], estimate.DefaultJobDuration.Mean)
	}
	if est.JobDurations[2] != estimate.DefaultJobDuration.Mean {
		t.Fatalf("job 2 duration = %v, want %v", est.JobDurations[2], estimate.DefaultJobDuration.Mean)
	}
}

func TestEstimateReuseGroup_ShrinksLowConfidenceUnknownRuntime(t *testing.T) {
	original := estimateJobDurationsDetailedForReuse
	t.Cleanup(func() { estimateJobDurationsDetailedForReuse = original })

	neutral := false
	estimateJobDurationsDetailedForReuse = func(_ *predictor.Config, batchJobs []predictor.BatchJob) map[int64]estimate.DurationPrediction {
		results := make(map[int64]estimate.DurationPrediction, len(batchJobs))
		for _, batchJob := range batchJobs {
			results[batchJob.ID] = estimate.DurationPrediction{
				Estimate: estimate.Constant(30 * time.Minute),
				Metadata: &predictor.RuntimeMetadata{
					Source:                     "learned",
					Confidence:                 0.7,
					Bottleneck:                 "unknown",
					MemoryHeadroomMiB:          128 * 1024,
					BenefitsFromAdditionalVRAM: &neutral,
				},
			}
		}
		return results
	}

	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs:     []*db.Job{{ID: 1}, {ID: 2}},
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{
			GPUClass:        "NVIDIA",
			ResolvedGPUName: "H200",
			GPUMemGB:        141,
			DLPerf:          40,
			InetDownMbps:    1000,
			InetUpMbps:      1000,
			Status:          db.LaunchStatusRunning,
		},
	}

	est, ok := EstimateReuseGroup(nil, group, cap, &predictor.Config{ProjectPath: "/tmp/job-estimator"}, nil)
	if !ok {
		t.Fatal("expected reuse estimate")
	}

	if est.Breakdown.Run.Mean <= time.Hour+45*time.Minute {
		t.Fatalf("Run.Mean = %v, want strong shrink toward neutral fallback", est.Breakdown.Run.Mean)
	}
	if est.JobDurations[1] <= 55*time.Minute {
		t.Fatalf("job 1 duration = %v, want shrink away from raw 30m prediction", est.JobDurations[1])
	}
}

func TestEstimateReuseGroup_RejectsEstimatorInfeasibleReuse(t *testing.T) {
	original := estimateJobDurationsDetailedForReuse
	t.Cleanup(func() { estimateJobDurationsDetailedForReuse = original })

	infeasible := false
	estimateJobDurationsDetailedForReuse = func(_ *predictor.Config, batchJobs []predictor.BatchJob) map[int64]estimate.DurationPrediction {
		results := make(map[int64]estimate.DurationPrediction, len(batchJobs))
		for _, batchJob := range batchJobs {
			results[batchJob.ID] = estimate.DurationPrediction{
				Estimate: estimate.Constant(30 * time.Minute),
				Metadata: &predictor.RuntimeMetadata{
					Source:   "learned",
					Feasible: &infeasible,
				},
			}
		}
		return results
	}

	group := InstanceGroup{
		GPUClass: "NVIDIA",
		Jobs:     []*db.Job{{ID: 1, Command: "python train.py --epochs 1"}},
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{
			GPUClass:        "NVIDIA",
			ResolvedGPUName: "RTX 4090",
			GPUMemGB:        24,
			InetDownMbps:    1000,
			InetUpMbps:      1000,
			Status:          db.LaunchStatusRunning,
		},
	}

	if _, ok := EstimateReuseGroup(nil, group, cap, &predictor.Config{ProjectPath: "/tmp/job-estimator"}, nil); ok {
		t.Fatal("expected estimator-infeasible reuse to be rejected")
	}
}

func TestTotalEstimatedCostFromEstimates(t *testing.T) {
	estimates := []CostEstimate{
		{TotalCost: 3.50},
		{TotalCost: 2.00},
		{TotalCost: 0}, // nil offer group
	}

	total := TotalEstimatedCostFromEstimates(estimates)
	if total < 5.49 || total > 5.51 {
		t.Errorf("total = %f, want ~5.50", total)
	}
}

func TestBudgetFromEstimate(t *testing.T) {
	tests := []struct {
		name         string
		est          CostEstimate
		wantMinSpend int
		wantMinTime  int
		wantMaxSpend int
		wantMaxTime  int
	}{
		{
			name:         "normal estimate uses 10x multiplier",
			est:          CostEstimate{TotalTime: 2 * time.Hour, TotalCost: 5.0},
			wantMinSpend: 5000,  // $5 * 10 * 100 = 5000 cents
			wantMinTime:  72000, // 2h * 10 = 20h = 72000s
			wantMaxSpend: 5000,
			wantMaxTime:  72000,
		},
		{
			name:         "small estimate hits floor",
			est:          CostEstimate{TotalTime: 10 * time.Minute, TotalCost: 0.10},
			wantMinSpend: MinBudgetCents,               // $20 floor
			wantMinTime:  int(MinBudgetTime.Seconds()), // 8h floor
			wantMaxSpend: MinBudgetCents,
			wantMaxTime:  int(MinBudgetTime.Seconds()),
		},
		{
			name:         "zero estimate hits floor",
			est:          CostEstimate{TotalTime: 0, TotalCost: 0},
			wantMinSpend: MinBudgetCents,
			wantMinTime:  int(MinBudgetTime.Seconds()),
			wantMaxSpend: MinBudgetCents,
			wantMaxTime:  int(MinBudgetTime.Seconds()),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spend, secs := BudgetFromEstimate(tt.est)
			if spend < tt.wantMinSpend {
				t.Errorf("MaxSpendCents = %d, want >= %d", spend, tt.wantMinSpend)
			}
			if spend > tt.wantMaxSpend {
				t.Errorf("MaxSpendCents = %d, want <= %d", spend, tt.wantMaxSpend)
			}
			if secs < tt.wantMinTime {
				t.Errorf("MaxTimeSeconds = %d, want >= %d", secs, tt.wantMinTime)
			}
			if secs > tt.wantMaxTime {
				t.Errorf("MaxTimeSeconds = %d, want <= %d", secs, tt.wantMaxTime)
			}
		})
	}
}

func TestFormatEstDuration(t *testing.T) {
	tests := []struct {
		dur           time.Duration
		hasPrediction bool
		wantContains  string
		wantExclude   string
	}{
		{90 * time.Minute, true, "~1h30", "est"},
		{2 * time.Hour, false, "(est)", ""},
		{30 * time.Second, true, "~30s", ""},
	}

	for _, tt := range tests {
		got := FormatEstDuration(tt.dur, tt.hasPrediction)
		if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
			t.Errorf("FormatEstDuration(%v, %v) = %q, want contains %q", tt.dur, tt.hasPrediction, got, tt.wantContains)
		}
		if tt.wantExclude != "" && strings.Contains(got, tt.wantExclude) {
			t.Errorf("FormatEstDuration(%v, %v) = %q, should not contain %q", tt.dur, tt.hasPrediction, got, tt.wantExclude)
		}
	}
}
