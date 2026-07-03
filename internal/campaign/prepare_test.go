package campaign

import (
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// insertBidLossHistory records n closed attempts for jobID, each on a fresh
// interruptible launch with an orphaned outcome — the signature of a lost
// bid instance.
func insertBidLossHistory(t *testing.T, database *sql.DB, jobID int64, n int) {
	t.Helper()
	now := time.Now().Unix()
	for i := 1; i <= n; i++ {
		launchID, err := db.CreateLaunch(database, &db.Launch{
			Status:       db.LaunchStatusFailed,
			Provider:     "vastai",
			InstanceType: cloud.InstanceTypeInterruptible,
		})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		if _, err := database.Exec(
			`INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, cloud_outcome, queued_at, end_time)
			 VALUES (?, ?, '', ?, ?, ?, ?, ?)`,
			jobID, i, launchID, db.StatusCanceled, db.AttemptOutcomeOrphaned, now, now,
		); err != nil {
			t.Fatalf("insert attempt %d: %v", i, err)
		}
	}
}

// TestApplyBidLossEscalation verifies the group-build integration: a group
// containing a preemptible job that has lost bidLossEscalationThreshold
// consecutive interruptible launches is flagged EscalateToOnDemand; groups
// below the threshold and non-preemptible groups are untouched.
func TestApplyBidLossEscalation(t *testing.T) {
	database := setupTestDB(t)

	insertJob := func(id int64) *db.Job {
		if _, err := database.Exec(
			`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'echo test', 0)`,
			id,
		); err != nil {
			t.Fatalf("insert job %d: %v", id, err)
		}
		return &db.Job{ID: id, Tags: []string{db.TagInterruptible}}
	}

	escalatedJob := insertJob(1)
	insertBidLossHistory(t, database, escalatedJob.ID, bidLossEscalationThreshold)

	belowThresholdJob := insertJob(2)
	insertBidLossHistory(t, database, belowThresholdJob.ID, bidLossEscalationThreshold-1)

	nonPreemptibleJob := insertJob(3)
	nonPreemptibleJob.Tags = nil
	insertBidLossHistory(t, database, nonPreemptibleJob.ID, bidLossEscalationThreshold)

	groups := []InstanceGroup{
		{GPUClass: "RTX_4090", Jobs: []*db.Job{escalatedJob}},
		{GPUClass: "RTX_4090", Jobs: []*db.Job{belowThresholdJob}},
		{GPUClass: "RTX_4090", Jobs: []*db.Job{nonPreemptibleJob}},
	}
	ApplyBidLossEscalation(database, groups)

	if !groups[0].EscalateToOnDemand {
		t.Errorf("group with %d consecutive bid losses: EscalateToOnDemand = false, want true", bidLossEscalationThreshold)
	}
	if groups[1].EscalateToOnDemand {
		t.Errorf("group with %d consecutive bid losses: EscalateToOnDemand = true, want false (below threshold)", bidLossEscalationThreshold-1)
	}
	if groups[2].EscalateToOnDemand {
		t.Error("non-preemptible group: EscalateToOnDemand = true, want false — escalation never touches non-preemptible groups")
	}

	// End to end: the escalated group must not opt into interruptible offers,
	// while the below-threshold group still does.
	if c := offerConstraintsForGroup(groups[0], 0.95); c.InstanceType == cloud.InstanceTypeInterruptible {
		t.Errorf("escalated group InstanceType = %q, want on-demand search", c.InstanceType)
	}
	if c := offerConstraintsForGroup(groups[1], 0.95); c.InstanceType != cloud.InstanceTypeInterruptible {
		t.Errorf("below-threshold group InstanceType = %q, want %q", c.InstanceType, cloud.InstanceTypeInterruptible)
	}
}

// TestApplyBidLossEscalation_SuccessBreaksChain verifies that escalation is
// per-launch-attempt with no persistent state: a completed attempt after the
// losses resets the consecutive count, so the next launch may bid again.
func TestApplyBidLossEscalation_SuccessBreaksChain(t *testing.T) {
	database := setupTestDB(t)
	if _, err := database.Exec(
		`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (1, '/tmp', 'echo test', 0)`,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	job := &db.Job{ID: 1, Tags: []string{db.TagInterruptible}}
	insertBidLossHistory(t, database, job.ID, bidLossEscalationThreshold)

	// A subsequent completed on-demand attempt breaks the chain.
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:       db.LaunchStatusCompleted,
		Provider:     "vastai",
		InstanceType: cloud.InstanceTypeOnDemand,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	now := time.Now().Unix()
	if _, err := database.Exec(
		`INSERT INTO job_attempts (job_id, attempt_number, host, launch_id, status, cloud_outcome, queued_at, end_time, exit_code)
		 VALUES (1, ?, '', ?, ?, ?, ?, ?, 0)`,
		bidLossEscalationThreshold+1, launchID, db.StatusCompleted, db.AttemptOutcomeCompleted, now, now,
	); err != nil {
		t.Fatalf("insert completed attempt: %v", err)
	}

	groups := []InstanceGroup{{GPUClass: "RTX_4090", Jobs: []*db.Job{job}}}
	ApplyBidLossEscalation(database, groups)
	if groups[0].EscalateToOnDemand {
		t.Error("EscalateToOnDemand = true after a successful run, want false — success breaks the consecutive chain")
	}
}
