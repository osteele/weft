package terminal

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// The consumer parses `weft list jobs --format json` rows, so the field has to
// survive the column pipeline, not merely exist on the struct.
func TestJobsJSONCarriesSubmitterSession(t *testing.T) {
	cols, err := ResolveColumns(nil, DefaultJSONColumnKeys)
	if err != nil {
		t.Fatalf("ResolveColumns: %v", err)
	}
	jobs := []*db.Job{
		{ID: 8001, Status: db.StatusQueued, SubmitterSession: "sess-abc-123"},
		{ID: 8002, Status: db.StatusQueued},
	}

	var buf bytes.Buffer
	if err := PrintJobsJSON(&buf, jobs, cols); err != nil {
		t.Fatalf("PrintJobsJSON: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal %q: %v", buf.String(), err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	got, ok := rows[0]["submitter_session"]
	if !ok {
		t.Fatalf("row is missing submitter_session; row = %v", rows[0])
	}
	if got != "sess-abc-123" {
		t.Fatalf("submitter_session = %v, want %q", got, "sess-abc-123")
	}
	// Unattributed jobs still carry the field, empty. A missing key would make
	// a consumer unable to tell "no session" from "old weft".
	empty, ok := rows[1]["submitter_session"]
	if !ok {
		t.Fatal("unattributed row must still carry the field")
	}
	if empty != "" {
		t.Fatalf("unattributed submitter_session = %v, want empty string", empty)
	}
}

func TestSubmitterSessionIsInDefaultJSONColumns(t *testing.T) {
	if !slices.Contains(DefaultJSONColumnKeys, "submitter_session") {
		t.Fatalf("DefaultJSONColumnKeys = %v, want submitter_session present", DefaultJSONColumnKeys)
	}
}

func TestJobsJSONCarriesKillAttribution(t *testing.T) {
	killedAt := time.Date(2026, 9, 6, 8, 15, 39, 0, time.UTC).Unix()
	cols, err := ResolveColumns(nil, DefaultJSONColumnKeys)
	if err != nil {
		t.Fatalf("ResolveColumns: %v", err)
	}
	jobs := []*db.Job{{
		ID:         6782,
		Status:     db.StatusKilled,
		KillActor:  "session:operator",
		KillReason: "clearing head-of-line blocker",
		KilledAt:   &killedAt,
	}}
	var buf bytes.Buffer
	if err := PrintJobsJSON(&buf, jobs, cols); err != nil {
		t.Fatalf("PrintJobsJSON: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal %q: %v", buf.String(), err)
	}
	row := rows[0]
	if row["kill_actor"] != "session:operator" || row["kill_reason"] != "clearing head-of-line blocker" {
		t.Fatalf("kill attribution missing from row: %v", row)
	}
	if row["killed_at"] != "2026-09-06T08:15:39Z" {
		t.Fatalf("killed_at = %v, want RFC3339 timestamp", row["killed_at"])
	}
}

func TestKillAttributionIsInDefaultJSONColumns(t *testing.T) {
	for _, key := range []string{"kill_actor", "kill_reason", "killed_at"} {
		if !slices.Contains(DefaultJSONColumnKeys, key) {
			t.Fatalf("DefaultJSONColumnKeys = %v, want %s present", DefaultJSONColumnKeys, key)
		}
	}
}
