package cmd

import (
	"math"
	"testing"
)

func TestQuantileNearestRank(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct {
		q    float64
		want float64
	}{
		{0.0, 1},   // floor
		{1.0, 10},  // ceiling
		{0.5, 5},   // ceil(0.5*10)=5 -> index 4 -> value 5
		{0.9, 9},   // ceil(0.9*10)=9 -> index 8 -> value 9
		{0.95, 10}, // ceil(0.95*10)=10 -> index 9 -> value 10
		{0.99, 10},
	}
	for _, c := range cases {
		if got := quantile(sorted, c.q); got != c.want {
			t.Errorf("quantile(q=%.2f) = %v, want %v", c.q, got, c.want)
		}
	}

	if got := quantile(nil, 0.99); got != 0 {
		t.Errorf("quantile(empty) = %v, want 0", got)
	}
	if got := quantile([]float64{42}, 0.99); got != 42 {
		t.Errorf("quantile(single) = %v, want 42", got)
	}
}

func TestSummarizeResiduals(t *testing.T) {
	if s := summarizeResiduals(nil); s.N != 0 {
		t.Fatalf("empty summary N = %d, want 0", s.N)
	}

	// Unsorted input must still produce sorted-order statistics.
	ratios := []float64{1.10, 1.00, 1.05, 1.30, 1.02}
	s := summarizeResiduals(ratios)
	if s.N != 5 {
		t.Fatalf("N = %d, want 5", s.N)
	}
	if s.Min != 1.00 {
		t.Errorf("Min = %v, want 1.00", s.Min)
	}
	if s.Max != 1.30 {
		t.Errorf("Max = %v, want 1.30", s.Max)
	}
	wantMean := (1.10 + 1.00 + 1.05 + 1.30 + 1.02) / 5
	if math.Abs(s.Mean-wantMean) > 1e-9 {
		t.Errorf("Mean = %v, want %v", s.Mean, wantMean)
	}
	// sorted: [1.00, 1.02, 1.05, 1.10, 1.30]; p50 = ceil(.5*5)=3 -> index 2 -> 1.05
	if s.P50 != 1.05 {
		t.Errorf("P50 = %v, want 1.05", s.P50)
	}
}

func TestTrustworthyLaunchTermination(t *testing.T) {
	cases := []struct {
		reason string
		want   bool
	}{
		{"", false},                 // non-terminal launch
		{"completed", true},         // ran its workload to completion
		{"job_failure", true},       // ran the workload, exited non-zero
		{"canceled", true},          // normal termination
		{"disk_full", false},        // truncated cache / capped peak
		{"provider_failure", false}, // never ran the workload
		{"bootstrap_timeout", false},
		{"infra_failure", false},
	}
	for _, c := range cases {
		if got := trustworthyLaunchTermination(c.reason); got != c.want {
			t.Errorf("trustworthyLaunchTermination(%q) = %v, want %v", c.reason, got, c.want)
		}
	}
}

func TestPlausiblePeak(t *testing.T) {
	const bound = 2_000_000_000_000 // 2 TB
	cases := []struct {
		summary, disk, want int64
	}{
		{10, 20, 20},              // larger valid component wins
		{bound + 1, 30, 30},       // reject poisoned summary, keep disk
		{40, bound + 1, 40},       // reject poisoned disk, keep summary
		{bound + 1, bound + 5, 0}, // both poisoned -> 0
		{0, 0, 0},                 // no data
	}
	for _, c := range cases {
		if got := plausiblePeak(c.summary, c.disk, bound); got != c.want {
			t.Errorf("plausiblePeak(%d, %d) = %d, want %d", c.summary, c.disk, got, c.want)
		}
	}
}
