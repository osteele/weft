package db

import (
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupOverheadTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return SetupTestDB(t)
}

func TestQueryOverheadObservations_Empty(t *testing.T) {
	db := setupOverheadTestDB(t)
	defer db.Close()

	obs, err := QueryOverheadObservations(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 0 {
		t.Errorf("expected 0 observations, got %d", len(obs))
	}
}

func TestQueryOverheadObservations_WithData(t *testing.T) {
	database := setupOverheadTestDB(t)
	defer database.Close()

	// Insert a completed cloud instance
	_, err := database.Exec(
		`INSERT INTO launches (id, status, provider, gpu_class, data_center, dl_perf, inet_down_mbps, inet_up_mbps, reliability, created_at, ready_at)
		 VALUES (1, 'completed', 'vastai', 'A100', 'US-East', 50.0, 800.0, 200.0, 0.99, 1000, 1045)`,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Insert a job associated with the instance
	insertTestJob(t, database, 10, "echo test", "/tmp", StatusCompleted, withLaunch(1))

	// Insert phase timings
	_, err = database.Exec(
		`INSERT INTO job_phase_timings (job_id, wrapper_start, setup_start, setup_end, upload_start, upload_end, cache_hf_bytes, cache_hf_post_bytes, uv_sync_seconds)
		 VALUES (10, 1050, 1052, 1082, 1200, 1215, 0, 5000000000, 12)`,
	)
	if err != nil {
		t.Fatal(err)
	}

	obs, err := QueryOverheadObservations(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(obs))
	}

	o := obs[0]
	if o.Provider != "vastai" {
		t.Errorf("provider = %q, want vastai", o.Provider)
	}
	if o.GPUClass != "A100" {
		t.Errorf("gpu_class = %q, want A100", o.GPUClass)
	}
	if o.DataCenter != "US-East" {
		t.Errorf("data_center = %q, want US-East", o.DataCenter)
	}

	// Startup: ready_at (1045) - created_at (1000) = 45s
	if o.StartupSec == nil || *o.StartupSec != 45 {
		t.Errorf("startup_sec = %v, want 45", o.StartupSec)
	}

	// SSH setup: wrapper_start (1050) - ready_at (1045) = 5s
	if o.SSHSetupSec == nil || *o.SSHSetupSec != 5 {
		t.Errorf("ssh_setup_sec = %v, want 5", o.SSHSetupSec)
	}

	// Job setup: setup_end (1082) - setup_start (1052) = 30s
	if o.JobSetupSec == nil || *o.JobSetupSec != 30 {
		t.Errorf("job_setup_sec = %v, want 30", o.JobSetupSec)
	}

	// Upload: upload_end (1215) - upload_start (1200) = 15s
	if o.UploadSec == nil || *o.UploadSec != 15 {
		t.Errorf("upload_sec = %v, want 15", o.UploadSec)
	}

	if o.CacheHFBytes == nil || *o.CacheHFBytes != 0 {
		t.Errorf("cache_hf_bytes = %v, want 0", o.CacheHFBytes)
	}
	if o.CacheHFPostBytes == nil || *o.CacheHFPostBytes != 5000000000 {
		t.Errorf("cache_hf_post_bytes = %v, want 5000000000", o.CacheHFPostBytes)
	}
}

func TestQueryOverheadObservations_SkipsNonCompleted(t *testing.T) {
	database := setupOverheadTestDB(t)
	defer database.Close()

	// Insert a running (not completed) instance
	_, err := database.Exec(
		`INSERT INTO launches (id, status, provider, created_at, ready_at) VALUES (1, 'running', 'vastai', 1000, 1045)`,
	)
	if err != nil {
		t.Fatal(err)
	}
	insertTestJob(t, database, 10, "echo", "/tmp", StatusRunning, withLaunch(1))
	startup := 45.0
	_, err = database.Exec(`INSERT INTO job_phase_timings (job_id, wrapper_start) VALUES (10, 1050)`)
	if err != nil {
		t.Fatal(err)
	}
	_ = startup

	obs, err := QueryOverheadObservations(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 0 {
		t.Errorf("expected 0 observations for non-completed instance, got %d", len(obs))
	}
}

func TestQueryOverheadObservations_NullTimestamps(t *testing.T) {
	database := setupOverheadTestDB(t)
	defer database.Close()

	// Instance without ready_at
	_, err := database.Exec(
		`INSERT INTO launches (id, status, provider, created_at) VALUES (1, 'completed', 'vastai', 1000)`,
	)
	if err != nil {
		t.Fatal(err)
	}
	insertTestJob(t, database, 10, "echo", "/tmp", StatusCompleted, withLaunch(1))
	_, err = database.Exec(`INSERT INTO job_phase_timings (job_id) VALUES (10)`)
	if err != nil {
		t.Fatal(err)
	}

	obs, err := QueryOverheadObservations(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(obs))
	}

	o := obs[0]
	if o.StartupSec != nil {
		t.Error("startup_sec should be nil without ready_at")
	}
	if o.SSHSetupSec != nil {
		t.Error("ssh_setup_sec should be nil without ready_at or wrapper_start")
	}
	if o.JobSetupSec != nil {
		t.Error("job_setup_sec should be nil without setup_start/setup_end")
	}
	if o.UploadSec != nil {
		t.Error("upload_sec should be nil without upload_start/upload_end")
	}
}
