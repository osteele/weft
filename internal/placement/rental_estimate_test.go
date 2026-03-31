package placement

import (
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	_ "modernc.org/sqlite"
)

func setupRentalDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Create launches table (minimal schema)
	_, err = db.Exec(`CREATE TABLE launches (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		campaign_id INTEGER,
		status TEXT NOT NULL DEFAULT 'planned',
		gpu_class TEXT,
		cost_per_hour_cents INTEGER,
		dl_perf REAL,
		inet_down_mbps REAL,
		created_at INTEGER NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}

	// Create host_data table for dataloc
	if err := dataloc.InitSchema(db); err != nil {
		t.Fatal(err)
	}
	// Create contention table
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS host_contention_obs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		host TEXT NOT NULL,
		gpu_pct INTEGER,
		cpu_pct INTEGER,
		queue_depth INTEGER,
		gpu_jobs_queued INTEGER,
		observed_at INTEGER NOT NULL
	)`)

	return db
}

func TestRecentRentalBaseline_NoData(t *testing.T) {
	db := setupRentalDB(t)
	b := recentRentalBaseline(db, "rtx3090")
	if b != nil {
		t.Error("expected nil baseline with no launch data")
	}
}

func TestRecentRentalBaseline_WithData(t *testing.T) {
	db := setupRentalDB(t)
	now := time.Now().Unix()
	// Insert some completed launches
	for i := 0; i < 3; i++ {
		_, _ = db.Exec(`INSERT INTO launches (status, gpu_class, cost_per_hour_cents, dl_perf, inet_down_mbps, created_at)
			VALUES ('completed', 'rtx3090', ?, ?, ?, ?)`,
			50+i*10, 10.0+float64(i), 500.0, now-int64(i*3600))
	}

	b := recentRentalBaseline(db, "rtx3090")
	if b == nil {
		t.Fatal("expected non-nil baseline")
	}
	if b.Count != 3 {
		t.Errorf("Count = %d, want 3", b.Count)
	}
	if b.CostPerHourCents == 0 {
		t.Error("CostPerHourCents should be non-zero")
	}
	if b.DLPerf == 0 {
		t.Error("DLPerf should be non-zero")
	}
}

func TestEstimateRentalCompletion_NoHistory(t *testing.T) {
	db := setupRentalDB(t)
	est := EstimateRentalCompletion(db, Constraints{GPUClass: "rtx3090"}, nil)
	if est != nil {
		t.Error("expected nil estimate with no historical data")
	}
}

func TestEstimateRentalCompletion_WithHistory(t *testing.T) {
	db := setupRentalDB(t)
	now := time.Now().Unix()
	for i := 0; i < 5; i++ {
		_, _ = db.Exec(`INSERT INTO launches (status, gpu_class, cost_per_hour_cents, dl_perf, inet_down_mbps, created_at)
			VALUES ('completed', 'rtx3090', 60, 12.0, 800.0, ?)`, now-int64(i*3600))
	}

	est := EstimateRentalCompletion(db, Constraints{GPUClass: "rtx3090"}, nil)
	if est == nil {
		t.Fatal("expected non-nil estimate")
	}
	if est.Total.Mean <= 0 {
		t.Error("Total.Mean should be positive")
	}
	if est.LaunchEst.Mean <= 0 {
		t.Error("LaunchEst should be positive")
	}
	if est.CostPerHour <= 0 {
		t.Error("CostPerHour should be positive")
	}
	// Total should be at least launch + run (even without transfer)
	if est.Total.Mean < est.LaunchEst.Mean+est.RunEst.Mean-time.Second {
		t.Errorf("Total (%.0fm) should be >= launch (%.0fm) + run (%.0fm)",
			est.Total.Mean.Minutes(), est.LaunchEst.Mean.Minutes(), est.RunEst.Mean.Minutes())
	}
}

func TestEstimateRentalCompletion_NilDB(t *testing.T) {
	est := EstimateRentalCompletion(nil, Constraints{GPUClass: "rtx3090"}, nil)
	if est != nil {
		t.Error("expected nil estimate with nil DB")
	}
}

func TestEstimateRentalCompletion_EmptyGPUClass(t *testing.T) {
	db := setupRentalDB(t)
	est := EstimateRentalCompletion(db, Constraints{}, nil)
	if est != nil {
		t.Error("expected nil estimate with empty GPU class")
	}
}

func TestRecentRentalBaseline_IgnoresOldLaunches(t *testing.T) {
	db := setupRentalDB(t)
	// Insert launches older than 7 days
	old := time.Now().Add(-8 * 24 * time.Hour).Unix()
	for i := 0; i < 3; i++ {
		_, _ = db.Exec(`INSERT INTO launches (status, gpu_class, cost_per_hour_cents, dl_perf, inet_down_mbps, created_at)
			VALUES ('completed', 'rtx3090', 60, 12.0, 800.0, ?)`, old-int64(i*3600))
	}
	b := recentRentalBaseline(db, "rtx3090")
	if b != nil {
		t.Error("expected nil baseline for old-only launches")
	}
}

func TestRecentRentalBaseline_IgnoresFailedLaunches(t *testing.T) {
	db := setupRentalDB(t)
	now := time.Now().Unix()
	// Insert failed launches only
	for i := 0; i < 3; i++ {
		_, _ = db.Exec(`INSERT INTO launches (status, gpu_class, cost_per_hour_cents, dl_perf, inet_down_mbps, created_at)
			VALUES ('failed', 'rtx3090', 60, 12.0, 800.0, ?)`, now-int64(i*3600))
	}
	b := recentRentalBaseline(db, "rtx3090")
	if b != nil {
		t.Error("expected nil baseline when only failed launches exist")
	}
}

func TestRecentRentalBaseline_DifferentGPUClasses(t *testing.T) {
	db := setupRentalDB(t)
	now := time.Now().Unix()
	// Insert launches for rtx4090
	for i := 0; i < 3; i++ {
		_, _ = db.Exec(`INSERT INTO launches (status, gpu_class, cost_per_hour_cents, dl_perf, inet_down_mbps, created_at)
			VALUES ('completed', 'rtx4090', 80, 20.0, 1000.0, ?)`, now-int64(i*3600))
	}
	// Query rtx3090 — should find nothing
	b := recentRentalBaseline(db, "rtx3090")
	if b != nil {
		t.Error("expected nil baseline for GPU class with no launches")
	}
	// Query rtx4090 — should find data
	b = recentRentalBaseline(db, "rtx4090")
	if b == nil {
		t.Error("expected non-nil baseline for rtx4090")
	}
}
