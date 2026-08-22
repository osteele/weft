package narrate

import (
	"reflect"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestLoadUnprocessedCountsAndViews(t *testing.T) {
	database := db.SetupTestDB(t)
	const submitterSession = "session-narrate-test"
	now := time.Now().Unix()
	exitZero := 0
	exitOne := 1

	completedID, err := db.RecordQueued(database, "cool30", "/tmp/augur", "echo ok", "completed")
	if err != nil {
		t.Fatalf("record completed: %v", err)
	}
	if err := db.SetJobProject(database, completedID, "augur"); err != nil {
		t.Fatalf("set completed project: %v", err)
	}
	if err := db.SetJobSubmitterSession(database, completedID, submitterSession); err != nil {
		t.Fatalf("set completed submitter session: %v", err)
	}
	if err := db.CloseAttempt(database, completedID, db.StatusCompleted, &exitZero, now); err != nil {
		t.Fatalf("close completed: %v", err)
	}

	failedID, err := db.RecordQueued(database, "cool30", "/tmp/augur", "false", "failed")
	if err != nil {
		t.Fatalf("record failed: %v", err)
	}
	if err := db.SetJobProject(database, failedID, "augur"); err != nil {
		t.Fatalf("set failed project: %v", err)
	}
	if err := db.SetJobSubmitterSession(database, failedID, submitterSession); err != nil {
		t.Fatalf("set failed submitter session: %v", err)
	}
	if err := db.CloseAttempt(database, failedID, db.StatusFailed, &exitOne, now); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	// Processed terminal jobs stay out of the unprocessed inbox.
	processedID, err := db.RecordQueued(database, "cool30", "/tmp/augur", "echo ok", "processed")
	if err != nil {
		t.Fatalf("record processed: %v", err)
	}
	if err := db.SetJobSubmitterSession(database, processedID, submitterSession); err != nil {
		t.Fatalf("set processed submitter session: %v", err)
	}
	if err := db.CloseAttempt(database, processedID, db.StatusCompleted, &exitZero, now); err != nil {
		t.Fatalf("close processed: %v", err)
	}
	if err := db.AddJobTag(database, processedID, db.ProcessedTag); err != nil {
		t.Fatalf("tag processed: %v", err)
	}

	// The combined loader derives counts and views from one inbox read, so
	// the views must cover exactly the jobs the counts describe.
	inbox, err := LoadUnprocessedCountsAndViews(database, "", submitterSession)
	if err != nil {
		t.Fatalf("LoadUnprocessedCountsAndViews: %v", err)
	}
	counts, views := inbox.Counts, inbox.Jobs
	if inbox.Scope.State != UnprocessedScopeScopedNonEmpty {
		t.Fatalf("scope = %q, want %q", inbox.Scope.State, UnprocessedScopeScopedNonEmpty)
	}
	if counts.Completed != 1 || counts.Failed != 1 {
		t.Fatalf("counts = %+v, want 1 completed / 1 failed", counts)
	}
	if !reflect.DeepEqual(counts.CompletedProjects, []string{"augur"}) ||
		!reflect.DeepEqual(counts.FailedProjects, []string{"augur"}) {
		t.Fatalf("projects = %+v / %+v, want [augur] for both", counts.CompletedProjects, counts.FailedProjects)
	}
	if len(views) != counts.Completed+counts.Failed {
		t.Fatalf("views = %d, want %d (one per counted job)", len(views), counts.Completed+counts.Failed)
	}
	if views[0].ID != completedID || views[1].ID != failedID {
		t.Fatalf("view IDs = [%d %d], want [%d %d]", views[0].ID, views[1].ID, completedID, failedID)
	}

	// The single-purpose loaders must agree with the combined one.
	countsOnly, err := LoadUnprocessedCounts(database, "", submitterSession)
	if err != nil {
		t.Fatalf("LoadUnprocessedCounts: %v", err)
	}
	if !reflect.DeepEqual(countsOnly, counts) {
		t.Fatalf("LoadUnprocessedCounts = %+v, combined loader reported %+v", countsOnly, counts)
	}
	viewsOnly, err := LoadUnprocessedJobViews(database, "", submitterSession)
	if err != nil {
		t.Fatalf("LoadUnprocessedJobViews: %v", err)
	}
	if len(viewsOnly.Jobs) != len(views) {
		t.Fatalf("LoadUnprocessedJobViews returned %d views, combined loader reported %d", len(viewsOnly.Jobs), len(views))
	}
	for i := range views {
		if viewsOnly.Jobs[i].ID != views[i].ID || viewsOnly.Jobs[i].Status != views[i].Status {
			t.Fatalf("view %d = %+v, combined loader reported %+v", i, viewsOnly.Jobs[i], views[i])
		}
	}
}
