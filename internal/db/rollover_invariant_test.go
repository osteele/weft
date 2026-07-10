package db

import (
	"database/sql"
	"testing"
	"time"
)

// The reset/requeue entry points are deliberately not collapsed into one
// primitive — each carries intentional differences (canceled preservation,
// pending_status three-way-merge seeding, cloud supersede, placement reasons).
// What they must all guarantee is the same, and it is the invariant the
// duplication actually threatens: after a reset the job presents as queued,
// there is at most one open attempt, and no open attempt still carries the
// stale terminal fields of the run being retried. This test pins that
// post-condition across every reset entry point so a future edit to any one of
// them cannot quietly leave a job wedged as failed/running.

// assertRolledOverToCleanQueued checks the shared reset post-condition.
func assertRolledOverToCleanQueued(t *testing.T, database *sql.DB, jobID int64) {
	t.Helper()
	job, err := GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job.Status != StatusQueued {
		t.Fatalf("job status after reset = %q, want %q", job.Status, StatusQueued)
	}
	attempts, err := ListAttempts(database, jobID)
	if err != nil {
		t.Fatalf("ListAttempts: %v", err)
	}
	open := 0
	for _, a := range attempts {
		if a.EndTime != nil {
			continue // closed attempt
		}
		open++
		if a.Status != StatusQueued {
			t.Fatalf("open attempt %d status = %q, want %q", a.ID, a.Status, StatusQueued)
		}
		if a.ExitCode != nil || a.ErrorMessage != "" || a.FailureReason != "" {
			t.Fatalf("open attempt %d still carries stale terminal fields: exit=%v err=%q failure=%q",
				a.ID, a.ExitCode, a.ErrorMessage, a.FailureReason)
		}
	}
	if open > 1 {
		t.Fatalf("job has %d open attempts after reset, want at most 1", open)
	}
}

func TestRolloverResetsToCleanQueuedState(t *testing.T) {
	// buildFailed drives a fresh job to a terminal failed attempt with stale
	// terminal fields (exit code, end time) still set.
	buildFailed := func(t *testing.T, database *sql.DB, host string) int64 {
		t.Helper()
		jobID, err := RecordQueued(database, host, "/tmp/project", "python train.py", "test")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		if _, err := CreateAttempt(database, jobID, host, nil, StatusRunning); err != nil {
			t.Fatalf("CreateAttempt: %v", err)
		}
		if err := CloseAttempt(database, jobID, StatusFailed, intPtr(1), time.Now().Unix()); err != nil {
			t.Fatalf("CloseAttempt: %v", err)
		}
		return jobID
	}
	buildFailedCloud := func(t *testing.T, database *sql.DB) int64 {
		t.Helper()
		jobID, err := RecordQueued(database, "", "/tmp/project", "python train.py", "test")
		if err != nil {
			t.Fatalf("RecordQueued: %v", err)
		}
		launchID, err := CreateLaunch(database, &Launch{Status: LaunchStatusFailed, Provider: "vastai"})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		if err := SetJobLaunchID(database, jobID, launchID); err != nil {
			t.Fatalf("SetJobLaunchID: %v", err)
		}
		if _, err := CreateAttempt(database, jobID, "", &launchID, StatusRunning); err != nil {
			t.Fatalf("CreateAttempt cloud: %v", err)
		}
		if err := CloseAttempt(database, jobID, StatusFailed, intPtr(1), time.Now().Unix()); err != nil {
			t.Fatalf("CloseAttempt cloud: %v", err)
		}
		return jobID
	}

	cases := []struct {
		name  string
		build func(t *testing.T, database *sql.DB) int64
		reset func(t *testing.T, database *sql.DB, jobID int64)
	}{
		{
			name:  "RequeueByID/onprem",
			build: func(t *testing.T, d *sql.DB) int64 { return buildFailed(t, d, "cool30") },
			reset: func(t *testing.T, d *sql.DB, id int64) {
				if err := RequeueByID(d, id); err != nil {
					t.Fatalf("RequeueByID: %v", err)
				}
			},
		},
		{
			name:  "RequeueByID/cloud",
			build: buildFailedCloud,
			reset: func(t *testing.T, d *sql.DB, id int64) {
				if err := RequeueByID(d, id); err != nil {
					t.Fatalf("RequeueByID: %v", err)
				}
			},
		},
		{
			name:  "RequeueFreshAttemptByTarget/unplaced",
			build: func(t *testing.T, d *sql.DB) int64 { return buildFailed(t, d, "cool30") },
			reset: func(t *testing.T, d *sql.DB, id int64) {
				if err := RequeueFreshAttemptByTarget(d, id, "", nil); err != nil {
					t.Fatalf("RequeueFreshAttemptByTarget: %v", err)
				}
			},
		},
		{
			name:  "MarkAttemptQueuedByID",
			build: func(t *testing.T, d *sql.DB) int64 { return buildFailed(t, d, "cool30") },
			reset: func(t *testing.T, d *sql.DB, id int64) {
				if err := MarkAttemptQueuedByID(d, id); err != nil {
					t.Fatalf("MarkAttemptQueuedByID: %v", err)
				}
			},
		},
		{
			name:  "ResetJobToUnplaced",
			build: func(t *testing.T, d *sql.DB) int64 { return buildFailed(t, d, "cool30") },
			reset: func(t *testing.T, d *sql.DB, id int64) {
				if err := ResetJobToUnplaced(d, id); err != nil {
					t.Fatalf("ResetJobToUnplaced: %v", err)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database := setupTestDB(t)
			jobID := tc.build(t, database)
			tc.reset(t, database, jobID)
			assertRolledOverToCleanQueued(t, database, jobID)
		})
	}
}
