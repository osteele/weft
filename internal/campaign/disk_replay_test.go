package campaign

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/osteele/weft/internal/db"
	_ "modernc.org/sqlite"
)

// TestReplayEstimateForJob reruns EstimateGroupDisk on a real job from the
// local jobs.db. Set WEFT_REPLAY_JOB_ID=<id> to run; otherwise skipped.
//
// Useful for diagnosing disk_full incidents: prints the estimator output
// alongside the recorded launch disk_gb and any captured peak telemetry.
func TestReplayEstimateForJob(t *testing.T) {
	jobIDStr := os.Getenv("WEFT_REPLAY_JOB_ID")
	if jobIDStr == "" {
		t.Skip("WEFT_REPLAY_JOB_ID not set")
	}
	jobID, err := strconv.ParseInt(jobIDStr, 10, 64)
	if err != nil {
		t.Fatalf("parse job id: %v", err)
	}

	home, _ := os.UserHomeDir()
	dbPath := filepath.Join(home, ".config/weft/jobs.db")
	if v := os.Getenv("WEFT_DB"); v != "" {
		dbPath = v
	}
	conn, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer conn.Close()

	job, err := db.GetJobByID(conn, jobID)
	if err != nil {
		t.Fatalf("load job: %v", err)
	}
	if job == nil {
		t.Fatalf("job %d not found", jobID)
	}

	memGB := 0
	if job.GPUMemGB != nil {
		memGB = *job.GPUMemGB
	}
	t.Logf("job %d project=%s gpu=%s gpu_mem=%dGB inputs=%v", job.ID, job.Project, job.GPU, memGB, job.Inputs)

	group := InstanceGroup{
		GPUClass: job.GPUClass,
		GPUMemGB: memGB,
		Jobs:     []*db.Job{job},
	}

	estimate, _ := EstimateGroupDisk(group, conn, nil)
	t.Logf("estimator: %d GB", estimate)

	// Recorded launch disk and offer host disk for any launch this job ran on.
	rows, err := conn.Query(`
		SELECT l.id, l.disk_gb, l.termination_reason, l.status, l.created_at
		  FROM launches l
		  JOIN job_attempts ja ON ja.launch_id = l.id
		 WHERE ja.job_id = ?`, jobID)
	if err != nil {
		t.Fatalf("query launches: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, diskGB, createdAt int64
		var reason, status sql.NullString
		if err := rows.Scan(&id, &diskGB, &reason, &status, &createdAt); err != nil {
			t.Fatal(err)
		}
		t.Logf("launch %d disk_gb=%d status=%s reason=%s",
			id, diskGB, status.String, reason.String)
	}

	// Peak observed disk usage across all attempts for this job.
	var peak sql.NullInt64
	_ = conn.QueryRow(
		`SELECT MAX(disk_total_bytes - disk_free_bytes)
		   FROM job_timeseries
		  WHERE job_id = ?`, jobID).Scan(&peak)
	if peak.Valid && peak.Int64 > 0 {
		t.Logf("telemetry peak disk used: %.2f GB", float64(peak.Int64)/1e9)
	} else {
		t.Logf("telemetry peak disk used: (no rows in job_timeseries)")
	}

	var phasePeak sql.NullInt64
	_ = conn.QueryRow(
		`SELECT MAX(disk_used_bytes) FROM job_phase_timings WHERE job_id = ?`,
		jobID).Scan(&phasePeak)
	if phasePeak.Valid && phasePeak.Int64 > 0 {
		t.Logf("phase_timings disk_used: %.2f GB", float64(phasePeak.Int64)/1e9)
	}

	fmt.Println()
}
