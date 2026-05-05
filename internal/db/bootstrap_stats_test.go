package db

import (
	"database/sql"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupBootstrapTestDB(t *testing.T) *sql.DB {
	t.Helper()
	return setupStatsTestDB(t)
}

// insertBootstrapLaunch inserts a launch that either bootstrapped or failed.
func insertBootstrapLaunch(t *testing.T, database *sql.DB, id int64, provider string, launchedAt int64, endedAt int64, status string) {
	t.Helper()
	_, err := database.Exec(
		`INSERT INTO launches (id, status, provider, launched_at, ended_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, status, provider, launchedAt, endedAt, launchedAt,
	)
	if err != nil {
		t.Fatal(err)
	}
}

// insertBootstrapJob inserts a job with wrapper_start phase timing, linked to a launch.
func insertBootstrapJob(t *testing.T, database *sql.DB, jobID, launchID, wrapperStart int64) {
	t.Helper()
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusCompleted, withLaunch(launchID))
	_, err := database.Exec(
		`INSERT INTO job_phase_timings (job_id, wrapper_start) VALUES (?, ?)`,
		jobID, wrapperStart,
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestComputeBootstrapSurvival_Empty(t *testing.T) {
	database := setupBootstrapTestDB(t)
	defer database.Close()

	s, err := ComputeBootstrapSurvival(database, "vastai")
	if err != nil {
		t.Fatal(err)
	}
	if s.SampleSize != 0 {
		t.Errorf("sample size = %d, want 0", s.SampleSize)
	}
	// Should return defaults when insufficient data
	if s.WarnAfter != 15*time.Minute {
		t.Errorf("warn = %v, want 15m (default)", s.WarnAfter)
	}
	if s.TerminateAfter != 20*time.Minute {
		t.Errorf("terminate = %v, want 20m (default)", s.TerminateAfter)
	}
}

func TestTotalBootstrapPercentile(t *testing.T) {
	database := setupBootstrapTestDB(t)
	defer database.Close()

	base := int64(1000000)
	for i, duration := range []int64{60, 120, 180, 240, 300} {
		launchID := int64(i + 1)
		insertBootstrapLaunch(t, database, launchID, "vastai", base, base+duration+10, "completed")
		insertBootstrapJob(t, database, launchID*10, launchID, base+duration)
	}

	got, samples, err := TotalBootstrapPercentile(database, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if samples != 5 {
		t.Fatalf("samples = %d, want 5", samples)
	}
	if got != 180*time.Second {
		t.Fatalf("p50 = %v, want 3m", got)
	}
}

func TestTotalBootstrapPercentileIgnoresFailuresAndInvalidDurations(t *testing.T) {
	database := setupBootstrapTestDB(t)
	defer database.Close()

	base := int64(1000000)
	insertBootstrapLaunch(t, database, 1, "vastai", base, base+100, "completed")
	insertBootstrapJob(t, database, 10, 1, base+90)
	insertBootstrapLaunch(t, database, 2, "vastai", base, base+200, "failed")
	insertBootstrapLaunch(t, database, 3, "vastai", base, base+300, "completed")
	insertBootstrapJob(t, database, 30, 3, base-10)

	got, samples, err := TotalBootstrapPercentile(database, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if samples != 1 {
		t.Fatalf("samples = %d, want 1", samples)
	}
	if got != 90*time.Second {
		t.Fatalf("p50 = %v, want 90s", got)
	}
}

func TestComputeBootstrapSurvival_InsufficientData(t *testing.T) {
	database := setupBootstrapTestDB(t)
	defer database.Close()

	// Insert 5 instances — below minimum of 20
	base := int64(1000000)
	for i := int64(1); i <= 5; i++ {
		insertBootstrapLaunch(t, database, i, "vastai", base, base+100, "completed")
		insertBootstrapJob(t, database, i*10, i, base+60)
	}

	s, err := ComputeBootstrapSurvival(database, "vastai")
	if err != nil {
		t.Fatal(err)
	}
	if s.SampleSize != 5 {
		t.Errorf("sample size = %d, want 5", s.SampleSize)
	}
	// Should return defaults
	if s.WarnAfter != 15*time.Minute {
		t.Errorf("warn = %v, want 15m (default)", s.WarnAfter)
	}
}

func TestComputeBootstrapSurvival_LearnedThresholds(t *testing.T) {
	database := setupBootstrapTestDB(t)
	defer database.Close()

	base := int64(1000000)

	// Create a distribution where P(success) drops gradually so warn and
	// terminate thresholds are distinct and above the floors.
	//
	// 10 fast bootstraps (60-120s)
	// 3 medium bootstraps (300-360s) — keeps P above 3% until ~6m
	// 2 slow bootstraps (420-480s) — keeps P above 3% until ~8m
	// 25 failures dying at 60-900s — dilute the success rate over time
	id := int64(1)
	jobID := int64(100)

	// Fast bootstraps (60-120s)
	for i := 0; i < 10; i++ {
		bootTime := int64(60 + i*6)
		insertBootstrapLaunch(t, database, id, "vastai", base, base+bootTime+100, "completed")
		insertBootstrapJob(t, database, jobID, id, base+bootTime)
		id++
		jobID++
	}

	// Medium bootstraps (300-360s)
	for i := 0; i < 3; i++ {
		bootTime := int64(300 + i*30)
		insertBootstrapLaunch(t, database, id, "vastai", base, base+bootTime+100, "completed")
		insertBootstrapJob(t, database, jobID, id, base+bootTime)
		id++
		jobID++
	}

	// Slow bootstraps (420-480s)
	for i := 0; i < 2; i++ {
		bootTime := int64(420 + i*30)
		insertBootstrapLaunch(t, database, id, "vastai", base, base+bootTime+100, "completed")
		insertBootstrapJob(t, database, jobID, id, base+bootTime)
		id++
		jobID++
	}

	// Failed bootstraps dying at various times (60-900s)
	for i := 0; i < 25; i++ {
		aliveTime := int64(60 + i*35)
		insertBootstrapLaunch(t, database, id, "vastai", base, base+aliveTime, "failed")
		id++
	}

	s, err := ComputeBootstrapSurvival(database, "vastai")
	if err != nil {
		t.Fatal(err)
	}
	if s.SampleSize != 40 {
		t.Errorf("sample size = %d, want 40", s.SampleSize)
	}

	// Warn should be somewhere between 3-10 minutes
	if s.WarnAfter < 3*time.Minute || s.WarnAfter > 10*time.Minute {
		t.Errorf("warn = %v, want between 3m and 10m", s.WarnAfter)
	}

	// Terminate should be after warn
	if s.TerminateAfter <= s.WarnAfter {
		t.Errorf("terminate (%v) should be after warn (%v)", s.TerminateAfter, s.WarnAfter)
	}

	// Terminate should be less than the old default of 20m given this data
	if s.TerminateAfter >= 20*time.Minute {
		t.Errorf("terminate = %v, should be less than 20m with this data", s.TerminateAfter)
	}
}

func TestComputeBootstrapSurvival_PerProvider(t *testing.T) {
	database := setupBootstrapTestDB(t)
	defer database.Close()

	base := int64(1000000)
	id := int64(1)
	jobID := int64(100)

	// 25 vastai instances: fast bootstraps
	for i := 0; i < 20; i++ {
		insertBootstrapLaunch(t, database, id, "vastai", base, base+200, "completed")
		insertBootstrapJob(t, database, jobID, id, base+60)
		id++
		jobID++
	}
	for i := 0; i < 5; i++ {
		insertBootstrapLaunch(t, database, id, "vastai", base, base+300, "failed")
		id++
	}

	// 25 runpod instances: slower bootstraps
	for i := 0; i < 20; i++ {
		insertBootstrapLaunch(t, database, id, "runpod", base, base+600, "completed")
		insertBootstrapJob(t, database, jobID, id, base+300)
		id++
		jobID++
	}
	for i := 0; i < 5; i++ {
		insertBootstrapLaunch(t, database, id, "runpod", base, base+700, "failed")
		id++
	}

	vastai, err := ComputeBootstrapSurvival(database, "vastai")
	if err != nil {
		t.Fatal(err)
	}
	runpod, err := ComputeBootstrapSurvival(database, "runpod")
	if err != nil {
		t.Fatal(err)
	}

	// runpod bootstraps are slower, so its thresholds should be higher
	if runpod.WarnAfter <= vastai.WarnAfter {
		t.Errorf("runpod warn (%v) should be > vastai warn (%v)", runpod.WarnAfter, vastai.WarnAfter)
	}
}

func TestComputeBootstrapSurvival_FloorEnforced(t *testing.T) {
	database := setupBootstrapTestDB(t)
	defer database.Close()

	base := int64(1000000)
	id := int64(1)
	jobID := int64(100)

	// All instances bootstrap in <30s — thresholds should still respect floor
	for i := 0; i < 15; i++ {
		insertBootstrapLaunch(t, database, id, "vastai", base, base+100, "completed")
		insertBootstrapJob(t, database, jobID, id, base+20)
		id++
		jobID++
	}
	// Add failures that die quickly too
	for i := 0; i < 10; i++ {
		insertBootstrapLaunch(t, database, id, "vastai", base, base+int64(10+i*5), "failed")
		id++
	}

	s, err := ComputeBootstrapSurvival(database, "vastai")
	if err != nil {
		t.Fatal(err)
	}

	if s.WarnAfter < 3*time.Minute {
		t.Errorf("warn = %v, should not be below 3m floor", s.WarnAfter)
	}
	if s.TerminateAfter < 5*time.Minute {
		t.Errorf("terminate = %v, should not be below 5m floor", s.TerminateAfter)
	}
}

func TestBootstrapDurations_ConditionalMedian(t *testing.T) {
	tests := []struct {
		name      string
		durations BootstrapDurations
		elapsed   time.Duration
		wantRem   time.Duration
		wantOK    bool
	}{
		{
			name:      "empty",
			durations: nil,
			elapsed:   10 * time.Second,
			wantOK:    false,
		},
		{
			name:      "insufficient tail",
			durations: BootstrapDurations{30 * time.Second, 60 * time.Second},
			elapsed:   25 * time.Second,
			wantOK:    false,
		},
		{
			name: "early elapsed, full distribution",
			// 5 durations: 30s, 60s, 90s, 120s, 150s
			// elapsed=5s → tail is all 5 → median = 90s → remaining = 85s
			durations: BootstrapDurations{
				30 * time.Second, 60 * time.Second, 90 * time.Second,
				120 * time.Second, 150 * time.Second,
			},
			elapsed: 5 * time.Second,
			wantRem: 85 * time.Second,
			wantOK:  true,
		},
		{
			name: "mid elapsed, trimmed tail",
			// elapsed=65s → tail is [90s, 120s, 150s] → median = 120s → remaining = 55s
			durations: BootstrapDurations{
				30 * time.Second, 60 * time.Second, 90 * time.Second,
				120 * time.Second, 150 * time.Second,
			},
			elapsed: 65 * time.Second,
			wantRem: 55 * time.Second,
			wantOK:  true,
		},
		{
			name: "past all durations",
			durations: BootstrapDurations{
				30 * time.Second, 60 * time.Second, 90 * time.Second,
			},
			elapsed: 100 * time.Second,
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rem, ok := tt.durations.ConditionalMedian(tt.elapsed)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && rem != tt.wantRem {
				t.Fatalf("remaining = %v, want %v", rem, tt.wantRem)
			}
		})
	}
}
