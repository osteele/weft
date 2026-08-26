package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func renderJobListJSONDocument(t *testing.T, database *sql.DB) map[string]any {
	t.Helper()
	var stdout string
	captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			if err := renderListPlain(database, nil); err != nil {
				t.Fatalf("renderListPlain: %v", err)
			}
		})
	})
	var document map[string]any
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("unmarshal job-list JSON %q: %v", stdout, err)
	}
	return document
}

func requireJobListJSONSelection(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	selection, ok := document["selection"].(map[string]any)
	if !ok {
		t.Fatalf("selection = %T(%v), want object", document["selection"], document["selection"])
	}
	return selection
}

func TestJobListJSONEnvelopeCarriesRealSelectionBounds(t *testing.T) {
	restoreListFlags(t)
	stubEmptyQueueStatus(t)
	database := db.SetupTestDB(t)

	oldID, err := db.RecordQueued(database, "studio", "/tmp/old", "echo old", "")
	if err != nil {
		t.Fatalf("record old job: %v", err)
	}
	old := time.Now().AddDate(0, 0, -30).Unix()
	if _, err := database.Exec(
		`UPDATE job_attempts SET start_time = ?, end_time = ?, status = 'completed', exit_code = 0 WHERE job_id = ?`,
		old, old, oldID); err != nil {
		t.Fatalf("age old job: %v", err)
	}
	for i := range 2 {
		if _, err := db.RecordQueued(database, "studio", fmt.Sprintf("/tmp/recent-%d", i), "echo recent", ""); err != nil {
			t.Fatalf("record recent job %d: %v", i, err)
		}
	}

	listFormat, listAllHosts, listLimit = "json", true, 1
	document := renderJobListJSONDocument(t, database)

	if document["kind"] != "job_list" {
		t.Errorf("kind = %v, want job_list", document["kind"])
	}
	if document["version"] != float64(1) {
		t.Errorf("version = %v, want 1", document["version"])
	}
	selection := requireJobListJSONSelection(t, document)
	for field, want := range map[string]any{
		"max_age_days": float64(defaultListMaxAgeDays),
		"limit":        float64(1),
		"older_hidden": float64(1),
		"limit_hidden": float64(1),
		"complete":     false,
	} {
		if selection[field] != want {
			t.Errorf("selection.%s = %v, want %v", field, selection[field], want)
		}
	}
	jobs, ok := document["jobs"].([]any)
	if !ok || len(jobs) != 1 {
		t.Fatalf("jobs = %T(%v), want one row", document["jobs"], document["jobs"])
	}
	row, ok := jobs[0].(map[string]any)
	if !ok {
		t.Fatalf("jobs[0] = %T(%v), want object", jobs[0], jobs[0])
	}
	for _, field := range []string{"id", "job_id", "status", "status_code"} {
		if _, ok := row[field]; !ok {
			t.Errorf("jobs[0] missing existing row field %q: %v", field, row)
		}
	}
}

func TestJobListJSONEnvelopeAlwaysCarriesCompleteUnboundedSelection(t *testing.T) {
	restoreListFlags(t)
	stubEmptyQueueStatus(t)
	database := db.SetupTestDB(t)
	if _, err := db.RecordQueued(database, "studio", "/tmp/queued", "echo queued", ""); err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	listFormat, listAllHosts, listAll, listLimit = "json", true, true, 0
	document := renderJobListJSONDocument(t, database)
	selection := requireJobListJSONSelection(t, document)

	if selection["max_age_days"] != nil {
		t.Errorf("selection.max_age_days = %v, want null with --all", selection["max_age_days"])
	}
	if selection["limit"] != nil {
		t.Errorf("selection.limit = %v, want null with --limit 0", selection["limit"])
	}
	if selection["older_hidden"] != float64(0) {
		t.Errorf("selection.older_hidden = %v, want 0", selection["older_hidden"])
	}
	if selection["limit_hidden"] != float64(0) {
		t.Errorf("selection.limit_hidden = %v, want 0", selection["limit_hidden"])
	}
	if selection["complete"] != true {
		t.Errorf("selection.complete = %v, want true", selection["complete"])
	}
}
