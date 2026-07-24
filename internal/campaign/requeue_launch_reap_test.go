package campaign

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// TestSupersededAttemptFreesLaunchForReaping is a regression test for the
// orphan-launch defect: `weft edit --retry` on a cloud job supersedes its
// attempt on the launch it was running on and requeues the job unplaced. That
// launch's only attempt then displays as "superseded". Launch-liveness
// accounting must treat a superseded (departed) attempt as terminal-and-neutral
// so the now-jobless rental is reaped, rather than believed busy forever.
func TestSupersededAttemptFreesLaunchForReaping(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUMemGB: 22})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	if _, err := database.Exec(`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'echo hi', 0)`, 5198); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if _, err := db.CreateAttempt(database, 5198, "", &launchID, db.StatusRunning); err != nil {
		t.Fatalf("create attempt: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET start_time = ? WHERE job_id = 5198 AND end_time IS NULL`, time.Now().Unix()); err != nil {
		t.Fatalf("mark started: %v", err)
	}

	// While the job is running, the launch is genuinely busy.
	jobs, _ := db.GetLaunchJobsIncludingAttempts(database, launchID)
	outcomes, _ := db.GetAttemptOutcomesByLaunch(database, launchID)
	if !hasActiveLaunchJobs(jobs, outcomes) {
		t.Fatal("running job: expected launch to have an active job")
	}
	if _, _, ok := ComputeJobState(jobs, outcomes).TerminalLaunchStatus(); ok {
		t.Fatal("running job: launch must not be reaped")
	}

	// `weft edit --retry`: requeue the running cloud job. This supersedes its
	// attempt on the launch and creates a fresh unplaced attempt.
	if err := db.RequeueByID(database, 5198); err != nil {
		t.Fatalf("RequeueByID: %v", err)
	}

	jobs, _ = db.GetLaunchJobsIncludingAttempts(database, launchID)
	outcomes, _ = db.GetAttemptOutcomesByLaunch(database, launchID)
	if outcomes[5198] != db.AttemptOutcomeSuperseded {
		t.Fatalf("attempt outcome on launch = %q, want superseded", outcomes[5198])
	}
	if hasActiveLaunchJobs(jobs, outcomes) {
		t.Fatal("after requeue: launch must have no active jobs (job migrated off)")
	}
	js := ComputeJobState(jobs, outcomes)
	if !js.AllJobsTerminal {
		t.Fatal("after requeue: AllJobsTerminal must be true so the launch is reapable")
	}
	status, reason, ok := js.TerminalLaunchStatus()
	if !ok {
		t.Fatal("after requeue: launch must be reapable (TerminalLaunchStatus ok)")
	}
	if status != db.LaunchStatusCompleted || reason != db.TerminationReasonCompleted {
		t.Fatalf("terminal status = %q/%q, want completed/completed (not a failure — the job was requeued, not failed)", status, reason)
	}
}
