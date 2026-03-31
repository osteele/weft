package placement

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupContentionDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	_, err = db.Exec(`CREATE TABLE host_contention_obs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		host TEXT NOT NULL,
		gpu_pct INTEGER,
		cpu_pct INTEGER,
		queue_depth INTEGER,
		gpu_jobs_queued INTEGER,
		observed_at INTEGER NOT NULL
	)`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestContentionFactor_WithLiveMetrics(t *testing.T) {
	db := setupContentionDB(t)
	m := &HostMetrics{GPUPercent: 80}
	cf := ContentionFactor(db, "host1", m)
	// Live: 1.0 + 80/100 = 1.8
	if cf != 1.8 {
		t.Errorf("ContentionFactor with 80%% GPU = %f, want 1.8", cf)
	}
}

func TestContentionFactor_NoMetrics_NoHistory(t *testing.T) {
	db := setupContentionDB(t)
	cf := ContentionFactor(db, "host1", nil)
	if cf != DefaultContentionFactor {
		t.Errorf("ContentionFactor with no data = %f, want %f", cf, DefaultContentionFactor)
	}
}

func TestContentionFactor_NoMetrics_WithHistory(t *testing.T) {
	db := setupContentionDB(t)
	now := time.Now().Unix()
	// Insert some recent observations
	for i := 0; i < 5; i++ {
		_, _ = db.Exec(`INSERT INTO host_contention_obs (host, gpu_pct, cpu_pct, queue_depth, gpu_jobs_queued, observed_at)
			VALUES (?, ?, ?, ?, ?, ?)`, "host1", 60, 30, 2, 1, now-int64(i*3600))
	}
	cf := ContentionFactor(db, "host1", nil)
	// Historical avg GPU = 60%, so factor = 1.0 + 0.6 = 1.6
	if cf != 1.6 {
		t.Errorf("ContentionFactor with historical data = %f, want 1.6", cf)
	}
}

func TestContentionFactor_IdleHost(t *testing.T) {
	db := setupContentionDB(t)
	m := &HostMetrics{GPUPercent: 0}
	cf := ContentionFactor(db, "host1", m)
	// Live metrics present with 0% GPU → factor 1.0 (no contention), no DB query
	if cf != 1.0 {
		t.Errorf("ContentionFactor with 0%% GPU = %f, want 1.0", cf)
	}
}

func TestRecordContentionObs(t *testing.T) {
	db := setupContentionDB(t)
	m := &HostMetrics{GPUPercent: 50, CPUPercent: 30, QueueDepth: 2, GPUJobsQueued: 1}
	RecordContentionObs(db, "host1", m)

	var count int
	db.QueryRow("SELECT COUNT(*) FROM host_contention_obs WHERE host = 'host1'").Scan(&count)
	if count != 1 {
		t.Errorf("expected 1 observation, got %d", count)
	}
}

func TestRecordContentionObs_NilSafe(t *testing.T) {
	db := setupContentionDB(t)
	// Should not panic
	RecordContentionObs(nil, "host1", &HostMetrics{GPUPercent: 50})
	RecordContentionObs(db, "host1", nil)
}

func TestRecentContentionStats_IgnoresOldData(t *testing.T) {
	db := setupContentionDB(t)
	// Insert very old observation (48h ago)
	old := time.Now().Add(-48 * time.Hour).Unix()
	_, _ = db.Exec(`INSERT INTO host_contention_obs (host, gpu_pct, cpu_pct, queue_depth, gpu_jobs_queued, observed_at)
		VALUES (?, ?, ?, ?, ?, ?)`, "host1", 90, 50, 5, 3, old)

	// Query for last 24h — should find nothing
	stats := RecentContentionStats(db, "host1", 24)
	if stats != nil {
		t.Errorf("expected nil stats for old-only data, got %+v", stats)
	}
}

func TestContentionFactor_FullyLoaded(t *testing.T) {
	db := setupContentionDB(t)
	m := &HostMetrics{GPUPercent: 100}
	cf := ContentionFactor(db, "host1", m)
	// 100% GPU → factor 2.0
	if cf != 2.0 {
		t.Errorf("ContentionFactor with 100%% GPU = %f, want 2.0", cf)
	}
}

func TestContentionFactor_DifferentHosts(t *testing.T) {
	db := setupContentionDB(t)
	now := time.Now().Unix()
	// host1 is busy historically
	for i := 0; i < 5; i++ {
		_, _ = db.Exec(`INSERT INTO host_contention_obs (host, gpu_pct, cpu_pct, queue_depth, gpu_jobs_queued, observed_at)
			VALUES ('host1', 80, 50, 3, 2, ?)`, now-int64(i*3600))
	}
	// host2 has no history
	cf1 := ContentionFactor(db, "host1", nil)
	cf2 := ContentionFactor(db, "host2", nil)

	if cf1 <= cf2 {
		t.Errorf("busy host contention (%f) should exceed unknown host contention (%f)", cf1, cf2)
	}
}
