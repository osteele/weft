package campaign

import (
	"slices"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// The runaway breaker pauses the autopilot's launches for unplaced work. A
// job already assigned to a host is never held by it, so neither the per-job
// lookup (`weft info`'s "Blocked by") nor `weft autopilot blocked` may name
// the breaker for one (wb173).
func TestRunawayBreakerAttributionExcludesPlacedJobs(t *testing.T) {
	database := db.SetupTestDB(t)
	unplacedID, err := db.RecordQueued(database, "", "/tmp", "echo unplaced", "unplaced")
	if err != nil {
		t.Fatalf("record unplaced: %v", err)
	}
	placedID, err := db.RecordQueued(database, "studio", "/tmp", "echo placed", "placed on studio")
	if err != nil {
		t.Fatalf("record placed: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		EventKind:  db.EventRelaunchRunawayTripped,
		OccurredAt: time.Now().Add(-time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("insert trip: %v", err)
	}

	lookup := func(id int64) *RunawayBreakerInfo {
		t.Helper()
		job, err := db.GetJobByID(database, id)
		if err != nil {
			t.Fatalf("get job: %v", err)
		}
		info, err := LookupRunawayBreakerForJob(database, job)
		if err != nil {
			t.Fatalf("lookup: %v", err)
		}
		return info
	}
	if lookup(unplacedID) == nil {
		t.Fatal("unplaced queued job: breaker not attributed, want the active global trip")
	}
	if info := lookup(placedID); info != nil {
		t.Fatalf("job assigned to studio: breaker attributed (%s), want none", info.ScopeLabel())
	}

	infos, err := LookupActiveRunawayBreakers(database)
	if err != nil || len(infos) != 1 {
		t.Fatalf("active breakers = %v (err %v), want 1", infos, err)
	}
	held, err := JobsBlockedByBreaker(database, infos[0])
	if err != nil {
		t.Fatalf("JobsBlockedByBreaker: %v", err)
	}
	if !slices.Contains(held, unplacedID) || slices.Contains(held, placedID) {
		t.Fatalf("held jobs = %v, want %d and not %d", held, unplacedID, placedID)
	}
}
