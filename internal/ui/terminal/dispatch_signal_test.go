package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/orchestration"
)

func TestDispatchNotProgressingStatus(t *testing.T) {
	database := db.SetupTestDB(t)
	id, err := db.RecordQueued(database, "test-host", "/tmp", "echo test", "stalled")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, base, id); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{JobID: id, EventKind: db.EventQueueDispatchFailed, Detail: "artifact staging failed", OccurredAt: base}); err != nil {
		t.Fatal(err)
	}
	// A newer backoff bookkeeping event must not hide the underlying condition.
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{JobID: id, EventKind: db.EventQueueDispatchDeferred, Detail: "dispatch backoff: retrying in 5m"}); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, id)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := &db.Job{ID: id + 1, Host: "test-host", Status: db.StatusQueued, Description: "ordinary"}
	orchestration.HydrateInventoryDispatchBlockedReasons(database, []*db.Job{job, ordinary})
	reason := blockreason.Resolve(job, blockreason.Options{Compact: true})
	if !reason.Blocked || reason.Kind != blockreason.KindBlocked || !strings.Contains(reason.Reason, "not progressing") {
		t.Fatalf("blocker = %+v", reason)
	}
	if blockreason.Resolve(ordinary, blockreason.Options{Compact: true}).Blocked {
		t.Fatal("ordinary queued job became blocked")
	}
	out := renderJobListGroupedStatusPlain([]*db.Job{job, ordinary}, 0)
	if !strings.Contains(out, "not progressing") || !strings.Contains(out, "artifact staging failed") {
		t.Fatalf("missing distinct status: %s", out)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{JobID: id, EventKind: db.EventQueueDispatchOK}); err != nil {
		t.Fatal(err)
	}
	job, err = db.GetJobByID(database, id)
	if err != nil {
		t.Fatal(err)
	}
	orchestration.HydrateInventoryDispatchBlockedReasons(database, []*db.Job{job})
	if job.QueueBlockedReason != "" {
		t.Fatalf("stale blocker after OK: %s", job.QueueBlockedReason)
	}
}
