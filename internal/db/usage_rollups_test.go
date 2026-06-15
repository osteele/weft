package db

import (
	"database/sql"
	"math"
	"testing"
	"time"
)

// insertUsageLaunch inserts a launch row with the given GPU count and runtime.
func insertUsageLaunch(t *testing.T, db *sql.DB, id int64, numGPUs int, launchedAt int64, endedAt *int64) {
	t.Helper()
	if _, err := db.Exec(
		`INSERT INTO launches (id, status, provider, num_gpus, launched_at, ended_at, created_at)
		 VALUES (?, 'completed', 'vastai', ?, ?, ?, ?)`,
		id, numGPUs, launchedAt, endedAt, launchedAt,
	); err != nil {
		t.Fatalf("insertUsageLaunch: %v", err)
	}
}

// setJobGPU stamps the CUDA_VISIBLE_DEVICES string on a job spec row.
func setJobGPU(t *testing.T, db *sql.DB, jobID int64, gpu string) {
	t.Helper()
	if _, err := db.Exec(`UPDATE jobs SET gpu = ? WHERE id = ?`, gpu, jobID); err != nil {
		t.Fatalf("setJobGPU: %v", err)
	}
}

func approxEqual(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestSumGPUHoursSince_CloudAndOnprem(t *testing.T) {
	database := SetupTestDB(t)
	now := time.Now().Unix()
	since := now - 24*3600

	// Cloud: 2 GPUs, ran from -2h to -1h → 1h × 2 = 2 GPU-h.
	endC := now - 1*3600
	insertUsageLaunch(t, database, 1, 2, now-2*3600, &endC)

	// On-prem: 4 GPUs ("0,1,2,3"), ran from -3h to -1h → 2h × 4 = 8 GPU-h.
	insertTestJob(t, database, 10, "echo onprem", "/tmp", StatusCompleted,
		withStartTime(now-3*3600), withEndTime(now-1*3600))
	setJobGPU(t, database, 10, "0,1,2,3")

	// A cloud job (launch_id set) with a GPU string must NOT be counted as
	// on-prem — its GPU time is already in the launches sum.
	insertTestJob(t, database, 11, "echo cloud-job", "/tmp", StatusCompleted,
		withLaunch(1), withStartTime(now-2*3600), withEndTime(now-1*3600))
	setJobGPU(t, database, 11, "0,1")

	cloud, onprem, err := SumGPUHoursSince(database, since)
	if err != nil {
		t.Fatalf("SumGPUHoursSince: %v", err)
	}
	if !approxEqual(cloud, 2.0, 0.01) {
		t.Errorf("cloud GPU-hours = %.3f, want ~2.0", cloud)
	}
	if !approxEqual(onprem, 8.0, 0.01) {
		t.Errorf("on-prem GPU-hours = %.3f, want ~8.0", onprem)
	}
}

func TestSumGPUHoursSince_ClipsToWindow(t *testing.T) {
	database := SetupTestDB(t)
	now := time.Now().Unix()
	since := now - 24*3600

	// Instance ran 48h ago to 1h ago with 1 GPU; only the last 23h (since→ended)
	// fall inside the 24h window → 23 GPU-h, not 47.
	endC := now - 1*3600
	insertUsageLaunch(t, database, 1, 1, now-48*3600, &endC)

	cloud, _, err := SumGPUHoursSince(database, since)
	if err != nil {
		t.Fatalf("SumGPUHoursSince: %v", err)
	}
	if !approxEqual(cloud, 23.0, 0.05) {
		t.Errorf("clipped cloud GPU-hours = %.3f, want ~23.0", cloud)
	}
}

func TestSumGPUHoursSince_RunningInstance(t *testing.T) {
	database := SetupTestDB(t)
	now := time.Now().Unix()
	since := now - 24*3600

	// Still-running instance (ended_at NULL), 1 GPU, launched 2h ago → ~2 GPU-h
	// accrued so far. now is computed inside the query, so allow tolerance.
	if _, err := database.Exec(
		`INSERT INTO launches (id, status, provider, num_gpus, launched_at, ended_at, created_at)
		 VALUES (1, 'running', 'vastai', 1, ?, NULL, ?)`,
		now-2*3600, now-2*3600,
	); err != nil {
		t.Fatalf("insert running launch: %v", err)
	}

	cloud, _, err := SumGPUHoursSince(database, since)
	if err != nil {
		t.Fatalf("SumGPUHoursSince: %v", err)
	}
	if !approxEqual(cloud, 2.0, 0.05) {
		t.Errorf("running cloud GPU-hours = %.3f, want ~2.0", cloud)
	}
}

func TestSumGPUHoursSince_Empty(t *testing.T) {
	database := SetupTestDB(t)
	cloud, onprem, err := SumGPUHoursSince(database, time.Now().Unix()-24*3600)
	if err != nil {
		t.Fatalf("SumGPUHoursSince: %v", err)
	}
	if cloud != 0 || onprem != 0 {
		t.Errorf("empty rollup = (%.3f, %.3f), want (0, 0)", cloud, onprem)
	}
}

func TestSumTransferBytesSince(t *testing.T) {
	database := SetupTestDB(t)
	now := time.Now().Unix()
	since := now - 24*3600

	// Cloud job ended 1h ago: HF cache grew 1GiB→5GiB (4GiB downloaded),
	// uploads 100MiB results + 50MiB workspace = 150MiB R2.
	insertTestJob(t, database, 10, "echo job", "/tmp", StatusCompleted,
		withStartTime(now-2*3600), withEndTime(now-1*3600))
	const gib = int64(1) << 30
	const mib = int64(1) << 20
	if _, err := database.Exec(
		`INSERT INTO job_phase_timings
		 (job_id, cache_hf_bytes, cache_hf_post_bytes, upload_results_bytes, upload_workspace_bytes)
		 VALUES (10, ?, ?, ?, ?)`,
		1*gib, 5*gib, 100*mib, 50*mib,
	); err != nil {
		t.Fatalf("insert phase timings: %v", err)
	}

	hf, r2, err := SumTransferBytesSince(database, since)
	if err != nil {
		t.Fatalf("SumTransferBytesSince: %v", err)
	}
	if hf != 4*gib {
		t.Errorf("HF download = %d, want %d", hf, 4*gib)
	}
	if r2 != 150*mib {
		t.Errorf("R2 upload = %d, want %d", r2, 150*mib)
	}
}

func TestSumTransferBytesSince_ClampsHFDeltaAndWindows(t *testing.T) {
	database := SetupTestDB(t)
	now := time.Now().Unix()
	since := now - 24*3600
	const gib = int64(1) << 30

	// Warm-cache job inside the window: post < pre → HF delta clamps to 0.
	insertTestJob(t, database, 10, "echo warm", "/tmp", StatusCompleted,
		withStartTime(now-2*3600), withEndTime(now-1*3600))
	if _, err := database.Exec(
		`INSERT INTO job_phase_timings (job_id, cache_hf_bytes, cache_hf_post_bytes, upload_results_bytes)
		 VALUES (10, ?, ?, ?)`,
		5*gib, 5*gib, 1*gib,
	); err != nil {
		t.Fatalf("insert warm timings: %v", err)
	}

	// Old job outside the 24h window (ended 48h ago) → excluded entirely.
	insertTestJob(t, database, 11, "echo old", "/tmp", StatusCompleted,
		withStartTime(now-49*3600), withEndTime(now-48*3600))
	if _, err := database.Exec(
		`INSERT INTO job_phase_timings (job_id, cache_hf_bytes, cache_hf_post_bytes, upload_results_bytes)
		 VALUES (11, ?, ?, ?)`,
		0, 10*gib, 10*gib,
	); err != nil {
		t.Fatalf("insert old timings: %v", err)
	}

	hf, r2, err := SumTransferBytesSince(database, since)
	if err != nil {
		t.Fatalf("SumTransferBytesSince: %v", err)
	}
	if hf != 0 {
		t.Errorf("HF download = %d, want 0 (warm clamp, old excluded)", hf)
	}
	if r2 != 1*gib {
		t.Errorf("R2 upload = %d, want %d (only the in-window job)", r2, 1*gib)
	}
}

func TestSumTransferBytesSince_Empty(t *testing.T) {
	database := SetupTestDB(t)
	hf, r2, err := SumTransferBytesSince(database, time.Now().Unix()-24*3600)
	if err != nil {
		t.Fatalf("SumTransferBytesSince: %v", err)
	}
	if hf != 0 || r2 != 0 {
		t.Errorf("empty transfer = (%d, %d), want (0, 0)", hf, r2)
	}
}
