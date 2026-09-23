package ops

import (
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestBatchSyncUnfencedCompletionPreservesRetryAndSyncsSibling(t *testing.T) {
	database := db.SetupTestDB(t)
	staleID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo retry", "")
	if err != nil {
		t.Fatal(err)
	}
	siblingID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo sibling", "")
	if err != nil {
		t.Fatal(err)
	}
	stale, err := db.GetJobByID(database, staleID)
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := db.GetJobByID(database, siblingID)
	if err != nil {
		t.Fatal(err)
	}
	original := syncJobStatusFromR2ForBatch
	t.Cleanup(func() { syncJobStatusFromR2ForBatch = original })
	syncJobStatusFromR2ForBatch = func(*sql.DB, *db.Job) (SyncResult, error) {
		return SyncResult{}, errors.New("no completion for current attempt")
	}
	now := time.Now()
	exitCode := 0
	statuses := map[int64]queueBatchStatus{
		staleID:   {ExitCode: &exitCode, Mtime: now.Add(-time.Hour).Unix(), FromR2: true},
		siblingID: {State: queueStateRunning, RunID: *sibling.LatestRunID, Mtime: now.Unix(), FromR2: true},
	}
	_, err = applyBatchStatusesAt(database, []int64{staleID, siblingID}, map[int64]*db.Job{staleID: stale, siblingID: sibling}, statuses, time.Second, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.GetJobByID(database, staleID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusQueued || got.ExitCode != nil || got.EndTime != nil {
		t.Fatalf("unfenced old completion overwrote retry: status=%s exit=%v end=%v", got.Status, got.ExitCode, got.EndTime)
	}
	got, err = db.GetJobByID(database, siblingID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusRunning {
		t.Fatalf("sibling status = %s", got.Status)
	}
}

func TestBatchSyncRowFailureDoesNotBlockSibling(t *testing.T) {
	database := db.SetupTestDB(t)
	badID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo bad", "")
	if err != nil {
		t.Fatal(err)
	}
	goodID, err := db.RecordQueued(database, "batch-host", "/tmp", "echo good", "")
	if err != nil {
		t.Fatal(err)
	}
	bad, err := db.GetJobByID(database, badID)
	if err != nil {
		t.Fatal(err)
	}
	good, err := db.GetJobByID(database, goodID)
	if err != nil {
		t.Fatal(err)
	}
	// A real write failure on one attempt must not discard another observation.
	_, err = database.Exec(`CREATE TRIGGER reject_one_sync BEFORE UPDATE ON job_attempts
		WHEN NEW.job_id = ` + strconv.FormatInt(badID, 10) + ` BEGIN SELECT RAISE(FAIL, 'injected row failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[int64]queueBatchStatus{
		badID:  {State: queueStateRunning, RunID: *bad.LatestRunID, Mtime: time.Now().Unix(), FromR2: true},
		goodID: {State: queueStateRunning, RunID: *good.LatestRunID, Mtime: time.Now().Unix(), FromR2: true},
	}
	_, err = applyBatchStatuses(database, []int64{badID, goodID}, map[int64]*db.Job{badID: bad, goodID: good}, statuses, time.Second)
	if err == nil {
		t.Fatal("row write error was hidden")
	}
	got, err := db.GetJobByID(database, goodID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != db.StatusRunning {
		t.Fatalf("sibling status = %s", got.Status)
	}
}
