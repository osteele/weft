package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

func TestLifecycleHookReceivesVersionedEventOnce(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "test-host", t.TempDir(), "true", "review worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobProject(database, jobID, "agent-review-worker"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobSubmitterSession(database, jobID, "agent-review-daemon/v1/state-a"); err != nil {
		t.Fatal(err)
	}
	exit := 0
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exit, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(t.TempDir(), "events.jsonl")
	cfg := &config.Config{Events: config.EventsConfig{Hooks: []config.EventHookConfig{{
		ID:      "capture",
		Command: []string{"sh", "-c", `cat >> "$1"; printf '%s\n' '{"schema_version":"weft-hook-ack/v1","disposition":"handled"}'`, "hook", out},
	}}}}
	dispatchLifecycleEvents(database, cfg, jobID)
	dispatchLifecycleEvents(database, cfg, jobID)

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("hook deliveries = %d, want queued and terminal: %q", len(lines), data)
	}
	events := make([]db.JobLifecycleEvent, len(lines))
	for i, line := range lines {
		if err := json.Unmarshal([]byte(line), &events[i]); err != nil {
			t.Fatal(err)
		}
	}
	queued, terminal := events[0], events[1]
	if queued.EventKind != "job.status_changed" || queued.Status != db.StatusQueued || queued.EventSequence != 1 {
		t.Fatalf("queued event = %+v", queued)
	}
	if terminal.SchemaVersion != db.JobEventSchemaVersion || terminal.JobID != "wj"+strconv.FormatInt(jobID, 10) || terminal.EventKind != "job.terminal" || terminal.Status != db.StatusCompleted || terminal.EventSequence != 2 {
		t.Fatalf("terminal event = %+v", terminal)
	}
	if terminal.SubmitterSession != "agent-review-daemon/v1/state-a" || terminal.AttemptID == 0 || terminal.AttemptNumber == 0 {
		t.Fatalf("event identity = %+v", terminal)
	}
}

func TestLifecycleHookFailuresDoNotBlockSibling(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "test-host", t.TempDir(), "true", "review worker")
	if err != nil {
		t.Fatal(err)
	}
	exit := 0
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exit, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "handled")
	cfg := &config.Config{Events: config.EventsConfig{Hooks: []config.EventHookConfig{
		{ID: "retrying", Command: []string{"sh", "-c", "cat >/dev/null; exit 2"}},
		{ID: "working", Command: []string{"sh", "-c", `cat >/dev/null; : > "$1"; printf '%s\n' '{"schema_version":"weft-hook-ack/v1","disposition":"handled"}'`, "hook", out}},
	}}}
	dispatchLifecycleEvents(database, cfg, jobID)
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("working hook did not run: %v", err)
	}
	states := map[string]string{}
	rows, err := database.Query(`SELECT hook_id, state FROM lifecycle_hook_deliveries`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatal(err)
		}
		states[id] = state
	}
	if states["retrying"] != "pending" || states["working"] != "delivered" {
		t.Fatalf("delivery states = %v", states)
	}
}

func TestLegacyNotificationIgnoresReservedAgentReviewSession(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "test-host", t.TempDir(), "true", "review worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobSubmitterSession(database, jobID, "agent-review-daemon/v1/state-a"); err != nil {
		t.Fatal(err)
	}
	exit := 0
	if err := db.CloseAttempt(database, jobID, db.StatusCompleted, &exit, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "legacy")
	cfg := &config.Config{Notifications: config.NotificationsConfig{Command: `: > "` + out + `"`}}
	dispatchLifecycleEvents(database, cfg, jobID)
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("legacy notification ran for reserved session: %v", err)
	}
	var disposition string
	if err := database.QueryRow(`SELECT disposition FROM lifecycle_hook_deliveries WHERE hook_id = ?`, legacyNotificationHookID).Scan(&disposition); err != nil {
		t.Fatal(err)
	}
	if disposition != "ignored" {
		t.Fatalf("legacy disposition = %q, want ignored", disposition)
	}
}
