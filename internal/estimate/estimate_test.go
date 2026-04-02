package estimate

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestConstant(t *testing.T) {
	e := Constant(5 * time.Minute)
	if e.Mean != 5*time.Minute || e.Lower != 5*time.Minute || e.Upper != 5*time.Minute {
		t.Errorf("Constant(5m) = %+v, want all bounds = 5m", e)
	}
}

func TestFromSeconds(t *testing.T) {
	e := FromSeconds(60, 30, 120)
	if e.Mean != time.Minute {
		t.Errorf("Mean = %v, want 1m", e.Mean)
	}
	if e.Lower != 30*time.Second {
		t.Errorf("Lower = %v, want 30s", e.Lower)
	}
	if e.Upper != 2*time.Minute {
		t.Errorf("Upper = %v, want 2m", e.Upper)
	}
}

func TestAdd(t *testing.T) {
	a := FromSeconds(10, 5, 20)
	b := FromSeconds(30, 15, 60)
	sum := a.Add(b)
	if sum.Mean != 40*time.Second {
		t.Errorf("Add Mean = %v, want 40s", sum.Mean)
	}
	// Quadrature: lower dev = sqrt(5^2 + 15^2) ≈ 15.81, so lower ≈ 24.19s
	if sum.Lower < 24*time.Second || sum.Lower > 25*time.Second {
		t.Errorf("Add Lower = %v, want ~24.2s (quadrature)", sum.Lower)
	}
	// Quadrature: upper dev = sqrt(10^2 + 30^2) ≈ 31.62, so upper ≈ 71.62s
	if sum.Upper < 71*time.Second || sum.Upper > 72*time.Second {
		t.Errorf("Add Upper = %v, want ~71.6s (quadrature)", sum.Upper)
	}
}

func TestZero(t *testing.T) {
	if !(Estimate{}).Zero() {
		t.Error("zero-value Estimate should be Zero()")
	}
	if Constant(time.Second).Zero() {
		t.Error("non-zero Estimate should not be Zero()")
	}
}

func TestTransferTime(t *testing.T) {
	// 1 GB at 100 MB/s = 10s mean, 5s lower, 20s upper
	e := TransferTime(1e9, 100e6)
	wantMean := 10 * time.Second
	if diff := e.Mean - wantMean; diff < -time.Millisecond || diff > time.Millisecond {
		t.Errorf("TransferTime Mean = %v, want %v", e.Mean, wantMean)
	}
	if e.Lower > e.Mean {
		t.Errorf("Lower %v should be <= Mean %v", e.Lower, e.Mean)
	}
	if e.Upper < e.Mean {
		t.Errorf("Upper %v should be >= Mean %v", e.Upper, e.Mean)
	}
}

func TestTransferTime_ZeroInputs(t *testing.T) {
	if !TransferTime(0, 100).Zero() {
		t.Error("zero bytes should give zero estimate")
	}
	if !TransferTime(1000, 0).Zero() {
		t.Error("zero bandwidth should give zero estimate")
	}
}

func TestEstimateStartup(t *testing.T) {
	e := EstimateStartup("vastai")
	if e.Mean != 45*time.Second {
		t.Errorf("startup Mean = %v, want 45s", e.Mean)
	}
	if e.Lower >= e.Mean {
		t.Error("Lower should be < Mean")
	}
	if e.Upper <= e.Mean {
		t.Error("Upper should be > Mean")
	}
}

func TestEstimateProvision(t *testing.T) {
	e := EstimateProvision(ProvisionInput{
		ModelDownloadBytes:   10 * 1024 * 1024 * 1024, // 10 GB
		BandwidthBytesPerSec: 100 * 1024 * 1024,       // 100 MB/s
	})
	if e.Zero() {
		t.Error("provision estimate should not be zero with model download")
	}
	// ~105s for 10 GB model + 500 MB workdir at 100 MB/s
	if e.Mean < 90*time.Second || e.Mean > 120*time.Second {
		t.Errorf("provision Mean = %v, want ~105s", e.Mean)
	}
}

func TestEstimateProvision_NoModel(t *testing.T) {
	e := EstimateProvision(ProvisionInput{
		BandwidthBytesPerSec: 100 * 1024 * 1024,
	})
	// Just workdir sync: 500 MB at 100 MB/s = ~5s
	if e.Mean < 4*time.Second || e.Mean > 6*time.Second {
		t.Errorf("provision Mean = %v, want ~5s", e.Mean)
	}
}

func TestEstimateJobDuration_NilConfig(t *testing.T) {
	job := &db.Job{ID: 1, Command: "echo hello"}
	est, hasPred := EstimateJobDuration(nil, "A100", job)
	if hasPred {
		t.Error("nil config should not produce a prediction")
	}
	if est.Mean != DefaultJobDuration.Mean {
		t.Errorf("fallback Mean = %v, want %v", est.Mean, DefaultJobDuration.Mean)
	}
}
