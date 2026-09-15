package ops

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func TestDispatchNotProgressingRuns(t *testing.T) {
	database := db.SetupTestDB(t)
	id, err := db.RecordQueued(database, "test-host", "/tmp", "echo test", "")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-10 * time.Hour).Truncate(time.Second)
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, base.Unix(), id); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, id)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(kind, detail string, at time.Time) {
		t.Helper()
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{JobID: id, EventKind: kind, Detail: detail, OccurredAt: at.Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	check := func(t *testing.T, at time.Time, want int) {
		t.Helper()
		signalDispatchNotProgressing(database, job, at)
		for _, table := range []string{"lifecycle_events", "job_lifecycle_events"} {
			var n int
			if err := database.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE job_id = ? AND event_kind = ?", id, db.EventQueueDispatchNotProgressing).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != want {
				t.Fatalf("%s events = %d, want %d", table, n, want)
			}
		}
	}
	insert(db.EventQueueDispatchFailed, "staging failed", base)
	t.Run("1_young", func(t *testing.T) { check(t, base.Add(59*time.Minute), 0); check(t, base.Add(time.Hour), 0) })
	t.Run("2_once", func(t *testing.T) {
		check(t, base.Add(time.Hour+time.Second), 1)
		insert(db.EventQueueDispatchFailed, "staging failed", base.Add(2*time.Hour))
		check(t, base.Add(3*time.Hour), 1)
	})
	t.Run("3_changed_detail", func(t *testing.T) {
		insert(db.EventQueueDispatchFailed, "source failed", base.Add(4*time.Hour))
		check(t, base.Add(5*time.Hour+time.Second), 2)
	})
	t.Run("3_after_OK", func(t *testing.T) {
		insert(db.EventQueueDispatchOK, "", base.Add(6*time.Hour))
		check(t, base.Add(7*time.Hour), 2)
		insert(db.EventQueueDispatchFailed, "source failed", base.Add(6*time.Hour))
		check(t, base.Add(7*time.Hour+time.Second), 3)
		check(t, base.Add(8*time.Hour), 3)
	})
}

func TestDispatchNotProgressingWiredBeforeBackoff(t *testing.T) {
	database := db.SetupTestDB(t)
	dir := t.TempDir()
	script, capture, cfgPath := filepath.Join(dir, "hook.sh"), filepath.Join(dir, "events.jsonl"), filepath.Join(dir, "config.toml")
	if err := os.WriteFile(script, []byte("cat >> \"$1\"\nprintf '%s\\n' '{\"schema_version\":\"weft-hook-ack/v1\",\"disposition\":\"handled\"}'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("[[events.hooks]]\nid = 'capture'\ncommand = ['sh', %q, %q]\n", script, capture)), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(config.SetConfigPathsForTesting(cfgPath, ""))
	id, err := db.RecordQueued(database, "test-host", "/tmp", "echo test", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, now.Add(-2*time.Hour).Unix(), id); err != nil {
		t.Fatal(err)
	}
	for _, at := range []time.Time{now.Add(-2 * time.Hour), now} {
		if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{JobID: id, EventKind: db.EventQueueDispatchFailed, Detail: "staging failed", OccurredAt: at.Unix()}); err != nil {
			t.Fatal(err)
		}
	}
	mockSSHFunc(t, func(string, string) (string, string, int) { return "", "", 0 })
	for range 2 {
		_, _, _ = ensureQueuedJobsOnRemote(database, "test-host", time.Second, time.Second, slog.Default())
	}
	var n int
	if err := database.QueryRow(`SELECT COUNT(*) FROM lifecycle_events WHERE job_id = ? AND event_kind = ?`, id, db.EventQueueDispatchNotProgressing).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	n = 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event db.JobLifecycleEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.EventKind == db.EventQueueDispatchNotProgressing {
			n++
			if event.SchemaVersion != db.JobEventSchemaVersion || event.Status != db.StatusQueued || event.AttemptID == 0 {
				t.Fatalf("notification = %+v", event)
			}
		}
	}
	if n != 1 {
		t.Fatalf("notifications = %d, want 1", n)
	}
}
