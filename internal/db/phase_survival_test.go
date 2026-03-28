package db

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestComputeSetupSurvival_Empty(t *testing.T) {
	database := setupStatsTestDB(t)
	defer database.Close()

	s, err := ComputeSetupSurvival(database, "uv run pytest", "/tmp/project")
	if err != nil {
		t.Fatal(err)
	}
	if s.SampleSize != 0 {
		t.Errorf("sample size = %d, want 0", s.SampleSize)
	}
	if s.WarnAfter != 15*time.Minute {
		t.Errorf("warn = %v, want 15m (default)", s.WarnAfter)
	}
	if s.TerminateAfter != 25*time.Minute {
		t.Errorf("terminate = %v, want 25m (default)", s.TerminateAfter)
	}
}

func insertSetupJob(t *testing.T, database *sql.DB, jobID, launchID int64, command, workingDir string, setupStart, setupEnd int64) {
	t.Helper()
	insertTestJob(t, database, jobID, command, workingDir, StatusCompleted, withLaunch(launchID))
	if setupEnd > 0 {
		if _, err := database.Exec(
			`INSERT INTO job_phase_timings (job_id, setup_start, setup_end) VALUES (?, ?, ?)`,
			jobID, setupStart, setupEnd,
		); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := database.Exec(
			`INSERT INTO job_phase_timings (job_id, setup_start) VALUES (?, ?)`,
			jobID, setupStart,
		); err != nil {
			t.Fatal(err)
		}
	}
}

func TestComputeSetupSurvival_SufficientData(t *testing.T) {
	database := setupStatsTestDB(t)
	defer database.Close()

	base := int64(1000000)
	id := int64(1)
	jobID := int64(100)

	// 15 fast setups (10-30s)
	for i := 0; i < 15; i++ {
		setupDur := int64(10 + i*2)
		insertBootstrapLaunch(t, database, id, "vastai", base, base+setupDur+100, "completed")
		insertSetupJob(t, database, jobID, id, "uv run pytest", "/tmp/project", base+5, base+5+setupDur)
		id++
		jobID++
	}

	// 5 medium setups (120-180s)
	for i := 0; i < 5; i++ {
		setupDur := int64(120 + i*15)
		insertBootstrapLaunch(t, database, id, "vastai", base, base+setupDur+100, "completed")
		insertSetupJob(t, database, jobID, id, "uv run pytest", "/tmp/project", base+5, base+5+setupDur)
		id++
		jobID++
	}

	// 10 failures (setup started but never ended)
	for i := 0; i < 10; i++ {
		aliveTime := int64(60 + i*30)
		insertBootstrapLaunch(t, database, id, "vastai", base, base+aliveTime, "failed")
		insertTestJob(t, database, jobID, "uv run pytest", "/tmp/project", StatusFailed, withLaunch(id))
		if _, err := database.Exec(
			`INSERT INTO job_phase_timings (job_id, setup_start) VALUES (?, ?)`,
			jobID, base+5,
		); err != nil {
			t.Fatal(err)
		}
		id++
		jobID++
	}

	s, err := ComputeSetupSurvival(database, "uv run pytest", "/tmp/project")
	if err != nil {
		t.Fatal(err)
	}
	if s.SampleSize < 20 {
		t.Errorf("sample size = %d, want >= 20", s.SampleSize)
	}
	if s.WarnAfter < 5*time.Minute {
		t.Errorf("warn = %v, should be >= 5m floor", s.WarnAfter)
	}
	if s.TerminateAfter <= s.WarnAfter {
		t.Errorf("terminate (%v) should be after warn (%v)", s.TerminateAfter, s.WarnAfter)
	}
}

func TestComputeSetupSurvival_FallbackToAllJobs(t *testing.T) {
	database := setupStatsTestDB(t)
	defer database.Close()

	base := int64(1000000)
	id := int64(1)
	jobID := int64(100)

	// 25 jobs with a different workspace — enough in aggregate
	for i := 0; i < 25; i++ {
		setupDur := int64(10 + i*5)
		insertBootstrapLaunch(t, database, id, "vastai", base, base+setupDur+100, "completed")
		insertSetupJob(t, database, jobID, id, "echo test", "/tmp/other-project", base+5, base+5+setupDur)
		id++
		jobID++
	}

	// Query with command/workspace that has no data
	s, err := ComputeSetupSurvival(database, "uv run pytest", "/tmp/my-project")
	if err != nil {
		t.Fatal(err)
	}
	if s.SampleSize < 20 {
		t.Errorf("sample size = %d, want >= 20 (from fallback to all jobs)", s.SampleSize)
	}
}

func TestComputeSetupSurvival_InsufficientData(t *testing.T) {
	database := setupStatsTestDB(t)
	defer database.Close()

	base := int64(1000000)
	for i := int64(1); i <= 5; i++ {
		insertBootstrapLaunch(t, database, i, "vastai", base, base+100, "completed")
		insertSetupJob(t, database, i*10, i, "echo test", "/tmp", base+5, base+15)
	}

	s, err := ComputeSetupSurvival(database, "echo test", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if s.WarnAfter != 15*time.Minute {
		t.Errorf("warn = %v, want 15m (default)", s.WarnAfter)
	}
	if s.TerminateAfter != 25*time.Minute {
		t.Errorf("terminate = %v, want 25m (default)", s.TerminateAfter)
	}
}

func TestComputeSetupSurvival_WorkspaceFallback(t *testing.T) {
	database := setupStatsTestDB(t)
	defer database.Close()

	base := int64(1000000)
	id := int64(1)
	jobID := int64(100)

	// 25 jobs with same workspace but different commands
	for i := 0; i < 25; i++ {
		setupDur := int64(10 + i*5)
		insertBootstrapLaunch(t, database, id, "vastai", base, base+setupDur+100, "completed")
		cmd := "echo test"
		if i%2 == 0 {
			cmd = "python train.py"
		}
		insertSetupJob(t, database, jobID, id, cmd, "/tmp/project", base+5, base+5+setupDur)
		id++
		jobID++
	}

	// Query with a specific command that has < 20 samples but workspace has >= 20
	s, err := ComputeSetupSurvival(database, "echo test", "/tmp/project")
	if err != nil {
		t.Fatal(err)
	}
	// Should use workspace-level data (tier 2), not command+workspace (tier 1)
	if s.SampleSize < 20 {
		t.Errorf("sample size = %d, want >= 20 (from workspace fallback)", s.SampleSize)
	}
}
