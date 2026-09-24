package cmd

import (
	"bytes"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

const (
	costTestLaunchedAt = int64(10_000)
	costTestEndedAt    = int64(13_600) // one hour of uptime
)

// finishedRentalLaunch creates a terminated rental instance billed at
// centsPerHour for exactly one hour.
func finishedRentalLaunch(t *testing.T, database *sql.DB, centsPerHour int) int64 {
	t.Helper()
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "L40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if _, err := database.Exec(`
		UPDATE launches
		   SET status = ?, launched_at = ?, ended_at = ?,
		       cost_per_hour_cents = ?, termination_reason = ?
		 WHERE id = ?`,
		db.LaunchStatusCompleted, costTestLaunchedAt, costTestEndedAt, centsPerHour,
		db.TerminationReasonCompleted, launchID,
	); err != nil {
		t.Fatalf("finalize launch: %v", err)
	}
	return launchID
}

// billedJobAcrossTwoRentals mirrors the shape of wj8605 in issue wb179: a job
// whose spend lives in its rental/attempt history, with nothing written to the
// stored per-attempt cost scalar. Total billed: $1.00.
func billedJobAcrossTwoRentals(t *testing.T, database *sql.DB, description string) int64 {
	t.Helper()
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", description)
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	first := finishedRentalLaunch(t, database, 50)
	second := finishedRentalLaunch(t, database, 50)

	if err := db.SetJobLaunchID(database, jobID, first); err != nil {
		t.Fatalf("SetJobLaunchID: %v", err)
	}
	exitCode := 1
	if err := db.CloseAttempt(database, jobID, db.StatusFailed, &exitCode, costTestEndedAt); err != nil {
		t.Fatalf("CloseAttempt: %v", err)
	}
	if _, err := db.CreateAttempt(database, jobID, "", &second, db.StatusFailed); err != nil {
		t.Fatalf("CreateAttempt: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Cost != nil && *job.Cost > 0 {
		t.Fatalf("fixture invalid: stored cost scalar is set (%v); the defect needs it absent", *job.Cost)
	}
	return jobID
}

func costJobsOutput(t *testing.T, database *sql.DB, all bool, limit int) string {
	t.Helper()
	var buf bytes.Buffer
	if err := reportJobCosts(&buf, database, all, limit, time.Unix(costTestEndedAt, 0)); err != nil {
		t.Fatalf("reportJobCosts: %v", err)
	}
	return buf.String()
}

// Spend recorded only in a job's rental history must be reported, not dropped
// because the stored scalar is empty (issue wb179).
func TestCostJobsReportsRentalHistorySpendWithoutStoredScalar(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID := billedJobAcrossTwoRentals(t, database, "cost history")

	out := costJobsOutput(t, database, false, 200)

	if !strings.Contains(out, ids.FormatJobID(jobID)) {
		t.Fatalf("billed job %s missing from cost report:\n%s", ids.FormatJobID(jobID), out)
	}
	if !strings.Contains(out, "$1.00") {
		t.Fatalf("expected $1.00 of rental spend in cost report:\n%s", out)
	}
	if strings.Contains(out, "No jobs with recorded cost") {
		t.Fatalf("billed job reported as having no cost:\n%s", out)
	}
}

// An empty result must name the selection that produced it, so "nothing
// selected" cannot read as "nothing spent".
func TestCostJobsEmptyResultNamesSelection(t *testing.T) {
	database := db.SetupTestDB(t)
	for range 3 {
		if _, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "echo hi", "no cost"); err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
	}

	out := costJobsOutput(t, database, false, 1)

	for _, want := range []string{
		"1 of 3 known job(s), active first then newest (--limit 1)",
		"No attributable cost among the 1 examined job(s)",
		"2 job(s) outside this selection",
		"--all",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("empty cost report missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "No jobs with recorded cost") {
		t.Fatalf("empty cost report used the unqualified message:\n%s", out)
	}
}

// A billed job outside the default window must be named as unexamined, and
// must be reported under --all.
func TestCostJobsAllFlagReachesBilledJobOutsideDefaultWindow(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID := billedJobAcrossTwoRentals(t, database, "old billed job")
	for range 2 {
		if _, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "echo hi", "newer"); err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
	}

	narrow := costJobsOutput(t, database, false, 1)
	if strings.Contains(narrow, ids.FormatJobID(jobID)) {
		t.Fatalf("job outside the --limit 1 selection was reported:\n%s", narrow)
	}
	if !strings.Contains(narrow, "2 job(s) outside this selection") {
		t.Fatalf("narrow selection did not report the unexamined jobs:\n%s", narrow)
	}

	wide := costJobsOutput(t, database, true, 1)
	if !strings.Contains(wide, ids.FormatJobID(jobID)) {
		t.Fatalf("--all did not report billed job %s:\n%s", ids.FormatJobID(jobID), wide)
	}
	if !strings.Contains(wide, "$1.00") {
		t.Fatalf("--all did not report the job's $1.00 of spend:\n%s", wide)
	}
	if !strings.Contains(wide, "all 3 known job(s)") {
		t.Fatalf("--all did not state its selection:\n%s", wide)
	}
	if strings.Contains(wide, "job(s) outside this selection") {
		t.Fatalf("--all claimed jobs were left unexamined:\n%s", wide)
	}
}

// A job that shared a rental instance with no attributable rate must report
// unknown, never $0.00.
func TestCostJobsUnattributableSharedRentalReportsUnknownNotZero(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "L40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	var jobIDs []int64
	for i := range 2 {
		jobID, err := db.RecordQueued(database, "", "/tmp/project", "python train.py", "shared rental")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
			t.Fatalf("SetJobLaunchID: %v", err)
		}
		exitCode := i
		if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exitCode, costTestEndedAt); err != nil {
			t.Fatalf("CloseAttempt: %v", err)
		}
		jobIDs = append(jobIDs, jobID)
	}

	out := costJobsOutput(t, database, true, 200)

	for _, jobID := range jobIDs {
		if !strings.Contains(out, ids.FormatJobID(jobID)) {
			t.Fatalf("job %s on a shared rental was omitted:\n%s", ids.FormatJobID(jobID), out)
		}
	}
	if !strings.Contains(out, "unknown") {
		t.Fatalf("shared-rental job did not report unknown cost:\n%s", out)
	}
	if strings.Contains(out, "$0.00") {
		t.Fatalf("unattributable cost was reported as zero:\n%s", out)
	}
}
