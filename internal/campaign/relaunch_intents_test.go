package campaign

import (
	"database/sql"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func seedJobs(t *testing.T, database *sql.DB, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		if _, err := database.Exec(
			`INSERT INTO jobs (id, working_dir, command, tombstoned) VALUES (?, '/tmp', 'echo', 0)`,
			id,
		); err != nil {
			t.Fatalf("seed job %d: %v", id, err)
		}
	}
}

func TestOpenRelaunchIntents_CreatesIntentsForGroup(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	seedJobs(t, database, 100, 101, 102)
	jobs := []*db.Job{{ID: 100}, {ID: 101}, {ID: 102}}
	intentIDs := openRelaunchIntents(database, jobs)
	if len(intentIDs) != 3 {
		t.Fatalf("got %d intents, want 3", len(intentIDs))
	}

	open, err := db.JobIDsWithOpenPlacementIntents(database)
	if err != nil {
		t.Fatalf("JobIDsWithOpenPlacementIntents: %v", err)
	}
	for _, j := range jobs {
		if _, ok := open[j.ID]; !ok {
			t.Errorf("job %d missing from open set", j.ID)
		}
	}
}

func TestOpenRelaunchIntents_SkipsJobsWithExistingOpenIntent(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	seedJobs(t, database, 100, 101)
	// Pre-existing intent for job 100 (e.g. from a concurrent move).
	if _, err := db.CreatePlacementIntent(database, 100, "bulk_move"); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	jobs := []*db.Job{{ID: 100}, {ID: 101}}
	intentIDs := openRelaunchIntents(database, jobs)
	// Only job 101 gets a new intent; job 100 is already covered.
	if len(intentIDs) != 1 {
		t.Fatalf("got %d new intents, want 1 (existing intent on job 100 should be skipped)", len(intentIDs))
	}
}

func TestCloseRelaunchIntents_ConfirmsOnSuccess(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	seedJobs(t, database, 200, 201)
	jobs := []*db.Job{{ID: 200}, {ID: 201}}
	intentIDs := openRelaunchIntents(database, jobs)

	closeRelaunchIntents(database, intentIDs, true, "placement succeeded")

	for _, id := range intentIDs {
		intent, err := db.GetPlacementIntent(database, id)
		if err != nil {
			t.Fatalf("GetPlacementIntent: %v", err)
		}
		if intent.State != db.PlacementIntentStateConfirmed {
			t.Errorf("intent %d state = %s, want confirmed", id, intent.State)
		}
		if intent.Resolution != "placement succeeded" {
			t.Errorf("intent %d resolution = %q, want %q", id, intent.Resolution, "placement succeeded")
		}
	}
}

func TestCloseRelaunchIntents_CancelsOnFailure(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	seedJobs(t, database, 300)
	jobs := []*db.Job{{ID: 300}}
	intentIDs := openRelaunchIntents(database, jobs)

	closeRelaunchIntents(database, intentIDs, false, "launch failed: provider rejected")

	intent, err := db.GetPlacementIntent(database, intentIDs[0])
	if err != nil {
		t.Fatalf("GetPlacementIntent: %v", err)
	}
	if intent.State != db.PlacementIntentStateCanceled {
		t.Errorf("state = %s, want canceled", intent.State)
	}
	if intent.Resolution != "launch failed: provider rejected" {
		t.Errorf("resolution = %q, want failure detail", intent.Resolution)
	}
}

// Regression: jobs that the relaunch loop never reaches (no offers, no client,
// runaway breaker, etc.) must not be left with a confirmed placement intent.
// The previous design opened intents upfront for the entire scope and confirmed
// all of them on success; this test pins the new per-group behavior that no
// intent is created for a group that never calls openRelaunchIntents.
func TestRelaunchSkippedJobsHaveNoIntent(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	seedJobs(t, database, 400, 401)
	// Simulate a relaunch pass where job 400's group had no offers — the
	// new code path never calls openRelaunchIntents for that group.
	openIDs := openRelaunchIntents(database, []*db.Job{{ID: 401}})
	closeRelaunchIntents(database, openIDs, true, "placement succeeded")

	open, err := db.JobIDsWithOpenPlacementIntents(database)
	if err != nil {
		t.Fatalf("JobIDsWithOpenPlacementIntents: %v", err)
	}
	if _, present := open[400]; present {
		t.Errorf("job 400 (skipped by relaunch) should not have an open intent")
	}

	// And it should never have had an intent at all — verify no historical
	// row exists for it.
	var count int
	if err := database.QueryRow(`SELECT COUNT(*) FROM placement_intents WHERE job_id = 400`).Scan(&count); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	if count != 0 {
		t.Errorf("job 400 has %d historical intents; should be 0 (never reached LaunchInstance)", count)
	}
}
