package terminal

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

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
