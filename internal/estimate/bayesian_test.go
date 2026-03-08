package estimate

import (
	"math"
	"testing"
	"time"
)

func TestFit_Nil_NoGroups(t *testing.T) {
	model := Fit(nil)
	if model != nil {
		t.Error("Fit(nil) should return nil")
	}

	model = Fit(map[string]*SuffStats{})
	if model != nil {
		t.Error("Fit(empty) should return nil")
	}
}

func TestFit_SingleGroup_ManyObs(t *testing.T) {
	// 20 observations around exp(3) ≈ 20s
	gs := &SuffStats{}
	for i := 0; i < 20; i++ {
		gs.Add(3.0 + 0.1*float64(i-10))
	}

	model := Fit(map[string]*SuffStats{"A": gs})
	if model == nil {
		t.Fatal("model should not be nil")
	}

	est := model.PredictiveEstimate("A")
	// Mean should be close to exp(3) ≈ 20s
	if est.Mean < 15*time.Second || est.Mean > 30*time.Second {
		t.Errorf("mean = %v, want ~20s", est.Mean)
	}
	// CI should be narrow with 20 observations
	ratio := float64(est.Upper) / float64(est.Lower)
	if ratio > 10 {
		t.Errorf("CI ratio = %.1f, want < 10 for 20 obs", ratio)
	}
}

func TestFit_SingleObs_HeavyShrinkage(t *testing.T) {
	// Global has 50 observations at log(30) = 3.4
	global := &SuffStats{}
	for i := 0; i < 50; i++ {
		global.Add(3.4 + 0.2*float64(i-25)/25)
	}

	// New group with single outlier observation at log(100) = 4.6
	single := &SuffStats{}
	single.Add(4.6)

	model := Fit(map[string]*SuffStats{"global": global, "outlier": single})
	if model == nil {
		t.Fatal("model should not be nil")
	}

	globalEst := model.PredictiveEstimate("global")
	outlierEst := model.PredictiveEstimate("outlier")

	// Single observation should be shrunk toward global mean
	// So outlier estimate should be between global and exp(4.6) ≈ 100s
	if outlierEst.Mean <= globalEst.Mean {
		t.Errorf("outlier mean %v should be > global mean %v", outlierEst.Mean, globalEst.Mean)
	}
	if outlierEst.Mean > 100*time.Second {
		t.Errorf("outlier mean %v should be < 100s (shrunk from 100s toward ~30s)", outlierEst.Mean)
	}

	// Outlier CI should be wider than global CI
	globalWidth := float64(globalEst.Upper - globalEst.Lower)
	outlierWidth := float64(outlierEst.Upper - outlierEst.Lower)
	if outlierWidth <= globalWidth {
		t.Errorf("outlier CI width %v should be > global CI width %v", outlierWidth, globalWidth)
	}
}

func TestPredictiveEstimate_UnknownGroup(t *testing.T) {
	gs := &SuffStats{}
	for i := 0; i < 10; i++ {
		gs.Add(3.0)
	}
	model := Fit(map[string]*SuffStats{"known": gs})
	if model == nil {
		t.Fatal("model should not be nil")
	}

	known := model.PredictiveEstimate("known")
	unknown := model.PredictiveEstimate("unknown_group")

	// Unknown group should return global prior, which with only one group
	// is similar to the known group
	if unknown.Mean == 0 {
		t.Error("unknown group should return non-zero estimate")
	}
	// Mean should be close to exp(3) ≈ 20s (same as known since only one group)
	if unknown.Mean < 10*time.Second || unknown.Mean > 40*time.Second {
		t.Errorf("unknown group mean = %v, want near exp(3) ≈ 20s", unknown.Mean)
	}
	_ = known // used for conceptual clarity
}

func TestFit_IdenticalGroups_ZeroTau(t *testing.T) {
	// All groups have same data → tau should be ~0
	groups := make(map[string]*SuffStats)
	for _, name := range []string{"A", "B", "C"} {
		gs := &SuffStats{}
		for i := 0; i < 10; i++ {
			gs.Add(3.0 + 0.05*float64(i-5))
		}
		groups[name] = gs
	}

	model := Fit(groups)
	if model == nil {
		t.Fatal("model should not be nil")
	}

	if model.Tau > 0.1 {
		t.Errorf("tau = %v, want ~0 for identical groups", model.Tau)
	}
}

func TestFit_TwoDifferentGroups_LargeTau(t *testing.T) {
	// Group A: log durations around 2 (exp(2) ≈ 7s)
	// Group B: log durations around 5 (exp(5) ≈ 148s)
	groupA := &SuffStats{}
	groupB := &SuffStats{}
	for i := 0; i < 20; i++ {
		groupA.Add(2.0 + 0.1*float64(i-10)/10)
		groupB.Add(5.0 + 0.1*float64(i-10)/10)
	}

	model := Fit(map[string]*SuffStats{"A": groupA, "B": groupB})
	if model == nil {
		t.Fatal("model should not be nil")
	}

	estA := model.PredictiveEstimate("A")
	estB := model.PredictiveEstimate("B")

	// With enough data and large group difference, estimates should stay distinct
	if estB.Mean <= estA.Mean {
		t.Errorf("group B mean %v should be > group A mean %v", estB.Mean, estA.Mean)
	}
	// They should be well separated
	ratio := float64(estB.Mean) / float64(estA.Mean)
	if ratio < 5 {
		t.Errorf("B/A ratio = %.1f, want > 5 for well-separated groups", ratio)
	}
}

func TestMonotonicity_MoreData_NarrowerCI(t *testing.T) {
	// Adding observations should tighten the CI
	gs1 := &SuffStats{}
	gs1.Add(3.0)
	model1 := Fit(map[string]*SuffStats{"A": gs1})

	gs10 := &SuffStats{}
	for i := 0; i < 10; i++ {
		gs10.Add(3.0 + 0.1*float64(i-5)/5)
	}
	model10 := Fit(map[string]*SuffStats{"A": gs10})

	if model1 == nil || model10 == nil {
		t.Fatal("models should not be nil")
	}

	est1 := model1.PredictiveEstimate("A")
	est10 := model10.PredictiveEstimate("A")

	width1 := float64(est1.Upper - est1.Lower)
	width10 := float64(est10.Upper - est10.Lower)

	if width10 >= width1 {
		t.Errorf("10-obs CI width %v should be < 1-obs CI width %v", width10, width1)
	}
}

func TestSuffStats(t *testing.T) {
	gs := &SuffStats{}
	gs.Add(1.0)
	gs.Add(2.0)
	gs.Add(3.0)

	if gs.N != 3 {
		t.Errorf("N = %d, want 3", gs.N)
	}
	if math.Abs(gs.Mean()-2.0) > 1e-10 {
		t.Errorf("Mean = %v, want 2.0", gs.Mean())
	}
	// Variance of {1,2,3} = 1.0
	if math.Abs(gs.Variance()-1.0) > 1e-10 {
		t.Errorf("Variance = %v, want 1.0", gs.Variance())
	}
}

func TestSuffStats_Empty(t *testing.T) {
	gs := &SuffStats{}
	if gs.Mean() != 0 {
		t.Error("empty Mean should be 0")
	}
	if gs.Variance() != 0 {
		t.Error("empty Variance should be 0")
	}
}

func TestSuffStats_SingleObs(t *testing.T) {
	gs := &SuffStats{}
	gs.Add(5.0)
	if gs.Mean() != 5.0 {
		t.Errorf("Mean = %v, want 5.0", gs.Mean())
	}
	// Variance of single observation is 0 (n < 2)
	if gs.Variance() != 0 {
		t.Error("single-obs Variance should be 0")
	}
}

func TestLognormalEstimate(t *testing.T) {
	// mu=3, predVar=0 → mean ≈ exp(3) ≈ 20.09s, lower=upper=mean
	est := lognormalEstimate(3.0, 0)
	expected := time.Duration(math.Exp(3.0) * float64(time.Second))
	if est.Mean != expected {
		t.Errorf("mean = %v, want %v", est.Mean, expected)
	}

	// With variance, lower < mean < upper
	est = lognormalEstimate(3.0, 1.0)
	if est.Lower >= est.Mean {
		t.Error("lower should be < mean with variance > 0")
	}
	if est.Upper <= est.Mean {
		t.Error("upper should be > mean with variance > 0")
	}
}
