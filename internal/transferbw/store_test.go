package transferbw

import (
	"database/sql"
	"math"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := InitSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestEMAConverges(t *testing.T) {
	db := setupTestDB(t)
	src := OnPremEndpoint("cool30")
	dst := OnPremEndpoint("cool100")
	bw := 100e6 // 100 MB/s

	for range 10 {
		if err := RecordObservation(db, src, dst, 100e6, 1*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	est, err := ComputeBandwidth(db, src.Key(), dst.Key())
	if err != nil {
		t.Fatal(err)
	}
	if est == nil {
		t.Fatal("expected estimate, got nil")
	}
	if est.ObservationCount != 10 {
		t.Errorf("count = %d, want 10", est.ObservationCount)
	}
	if math.Abs(est.EMABytesPerSec-bw)/bw > 0.01 {
		t.Errorf("EMA = %.0f, want ~%.0f", est.EMABytesPerSec, bw)
	}
	if est.EMAVariance > 1e6 {
		t.Errorf("variance = %.0f, want ~0", est.EMAVariance)
	}
}

func TestEMAAdapts(t *testing.T) {
	db := setupTestDB(t)
	src := HFEndpoint()
	dst := OnPremEndpoint("cool100")

	for range 5 {
		if err := RecordObservation(db, src, dst, 50e6, 1*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	est1, _ := ComputeBandwidth(db, src.Key(), dst.Key())
	bw1 := est1.EMABytesPerSec

	for range 10 {
		if err := RecordObservation(db, src, dst, 200e6, 1*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	est2, _ := ComputeBandwidth(db, src.Key(), dst.Key())
	bw2 := est2.EMABytesPerSec

	if bw2 <= bw1 {
		t.Errorf("EMA should increase: was %.0f, now %.0f", bw1, bw2)
	}
	if bw2 < 150e6 {
		t.Errorf("EMA = %.0f, expected > 150M after shift to 200M", bw2)
	}
}

func TestEffectiveBandwidth_FallbackToStatic(t *testing.T) {
	db := setupTestDB(t)
	src := OnPremEndpoint("cool30")
	dst := OnPremEndpoint("cool100")
	staticBW := 125e6 // 1 Gbps

	// No observations → static
	got := EffectiveBandwidth(db, src.Key(), dst.Key(), staticBW)
	if got != staticBW {
		t.Errorf("got %.0f, want %.0f (static)", got, staticBW)
	}

	// 1 observation -> still below MinObservations
	if err := RecordObservation(db, src, dst, 200e6, 1*time.Second); err != nil {
		t.Fatal(err)
	}
	got = EffectiveBandwidth(db, src.Key(), dst.Key(), staticBW)
	if got != staticBW {
		t.Errorf("got %.0f, want %.0f (static, only 1 obs)", got, staticBW)
	}

	// 2nd observation → learned
	if err := RecordObservation(db, src, dst, 200e6, 1*time.Second); err != nil {
		t.Fatal(err)
	}
	got = EffectiveBandwidth(db, src.Key(), dst.Key(), staticBW)
	if got == staticBW {
		t.Errorf("got static BW, expected learned after 2 observations")
	}
}

func TestRecordObservation_ZeroDurationIsNoop(t *testing.T) {
	db := setupTestDB(t)
	src := OnPremEndpoint("cool30")
	dst := OnPremEndpoint("cool100")

	if err := RecordObservation(db, src, dst, 100, 0); err != nil {
		t.Fatal(err)
	}
	est, _ := ComputeBandwidth(db, src.Key(), dst.Key())
	if est != nil {
		t.Error("expected nil for zero duration")
	}
}

func TestRecordObservation_ZeroBytesIsNoop(t *testing.T) {
	db := setupTestDB(t)
	src := OnPremEndpoint("cool30")
	dst := OnPremEndpoint("cool100")

	if err := RecordObservation(db, src, dst, 0, 1*time.Second); err != nil {
		t.Fatal(err)
	}
	est, _ := ComputeBandwidth(db, src.Key(), dst.Key())
	if est != nil {
		t.Error("expected nil for zero bytes")
	}
}

func TestRecordObservation_NegativeDurationIsNoop(t *testing.T) {
	db := setupTestDB(t)
	src := OnPremEndpoint("cool30")
	dst := OnPremEndpoint("cool100")

	if err := RecordObservation(db, src, dst, 100, -1*time.Second); err != nil {
		t.Fatal(err)
	}
	est, _ := ComputeBandwidth(db, src.Key(), dst.Key())
	if est != nil {
		t.Error("expected nil for negative duration")
	}
}

func TestEndpointKeys(t *testing.T) {
	tests := []struct {
		endpoint Endpoint
		want     string
	}{
		{HFEndpoint(), "hf"},
		{R2Endpoint(), "r2"},
		{CloudEndpoint("vastai", "us-east-1", "12345"), "cloud:vastai:us-east-1"},
		{OnPremEndpoint("cool30"), "onprem:cool30"},
	}
	for _, tt := range tests {
		got := tt.endpoint.Key()
		if got != tt.want {
			t.Errorf("Key() = %q, want %q", got, tt.want)
		}
	}
}

func TestRecordObservation_StoresProvenance(t *testing.T) {
	db := setupTestDB(t)
	src := CloudEndpoint("vastai", "us-east-1", "src-999")
	dst := CloudEndpoint("vastai", "us-east-1", "dst-888")

	if err := RecordObservation(db, src, dst, 500e6, 2*time.Second); err != nil {
		t.Fatal(err)
	}

	var srcInstID, dstInstID string
	err := db.QueryRow(
		`SELECT source_instance_id, dest_instance_id FROM transfer_observations LIMIT 1`,
	).Scan(&srcInstID, &dstInstID)
	if err != nil {
		t.Fatal(err)
	}
	if srcInstID != "src-999" {
		t.Errorf("source_instance_id = %q, want %q", srcInstID, "src-999")
	}
	if dstInstID != "dst-888" {
		t.Errorf("dest_instance_id = %q, want %q", dstInstID, "dst-888")
	}
}

func TestObservationCount(t *testing.T) {
	db := setupTestDB(t)
	src := OnPremEndpoint("cool30")
	dst := OnPremEndpoint("cool100")

	if n := ObservationCount(db, src.Key(), dst.Key()); n != 0 {
		t.Errorf("count = %d, want 0", n)
	}

	for range 3 {
		if err := RecordObservation(db, src, dst, 100e6, 1*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	if n := ObservationCount(db, src.Key(), dst.Key()); n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
}

func TestSeparateEndpointPairsAreIndependent(t *testing.T) {
	db := setupTestDB(t)

	// Fast transfers from HF
	for range 3 {
		if err := RecordObservation(db, HFEndpoint(), OnPremEndpoint("cool100"), 500e6, 1*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	// Slow transfers from onprem
	for range 3 {
		if err := RecordObservation(db, OnPremEndpoint("cool30"), OnPremEndpoint("cool100"), 50e6, 1*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	hfEst, _ := ComputeBandwidth(db, "hf", "onprem:cool100")
	rsyncEst, _ := ComputeBandwidth(db, "onprem:cool30", "onprem:cool100")

	if hfEst.EMABytesPerSec <= rsyncEst.EMABytesPerSec {
		t.Errorf("HF BW (%.0f) should be higher than rsync BW (%.0f)",
			hfEst.EMABytesPerSec, rsyncEst.EMABytesPerSec)
	}
}
