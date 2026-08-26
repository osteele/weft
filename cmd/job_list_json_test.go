package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
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

func requireJobListJSONConstraints(t *testing.T, selection map[string]any) []any {
	t.Helper()
	constraints, ok := selection["constraints"].([]any)
	if !ok {
		t.Fatalf("selection.constraints = %T(%v), want list", selection["constraints"], selection["constraints"])
	}
	return constraints
}

func requireJobListJSONConstraint(t *testing.T, constraints []any, kind string) map[string]any {
	t.Helper()
	for _, raw := range constraints {
		constraint, ok := raw.(map[string]any)
		if ok && constraint["kind"] == kind {
			return constraint
		}
	}
	t.Fatalf("constraint %q not found in %v", kind, constraints)
	return nil
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
	if selection["complete"] != false {
		t.Errorf("selection.complete = %v, want false", selection["complete"])
	}
	if selection["order"] != string(jobListJSONOrderNewestFirst) {
		t.Errorf("selection.order = %v, want %q", selection["order"], jobListJSONOrderNewestFirst)
	}
	constraints := requireJobListJSONConstraints(t, selection)
	if len(constraints) != 2 {
		t.Fatalf("selection.constraints = %v, want max-age and limit only", constraints)
	}
	for _, tc := range []struct {
		kind   jobListJSONConstraintKind
		value  int
		hidden int
	}{
		{jobListJSONConstraintMaxAgeDays, defaultListMaxAgeDays, 1},
		{jobListJSONConstraintLimit, 1, 1},
	} {
		constraint := requireJobListJSONConstraint(t, constraints, string(tc.kind))
		if constraint["value"] != float64(tc.value) || constraint["hidden"] != float64(tc.hidden) || constraint["requested"] != true {
			t.Errorf("constraint %q = %v, want value=%d hidden=%d requested=true", tc.kind, constraint, tc.value, tc.hidden)
		}
	}
	for _, oldField := range []string{"max_age_days", "limit", "older_hidden", "limit_hidden"} {
		if _, ok := selection[oldField]; ok {
			t.Errorf("selection retains old field %q: %v", oldField, selection)
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

	constraints := requireJobListJSONConstraints(t, selection)
	if len(constraints) != 0 {
		t.Errorf("selection.constraints = %v, want empty list", constraints)
	}
	if selection["complete"] != true {
		t.Errorf("selection.complete = %v, want true", selection["complete"])
	}
	if selection["order"] != string(jobListJSONOrderNewestFirst) {
		t.Errorf("selection.order = %v, want %q", selection["order"], jobListJSONOrderNewestFirst)
	}
}

func TestJobListJSONEnvelopeCountsImplicitHostSyncWindow(t *testing.T) {
	restoreListFlags(t)
	stubEmptyQueueStatus(t)
	database := db.SetupTestDB(t)

	if _, err := db.RecordQueued(database, "fresh-host", "/tmp/fresh", "echo match", ""); err != nil {
		t.Fatalf("record fresh-host job: %v", err)
	}
	if _, err := db.RecordQueued(database, "stale-host", "/tmp/stale", "echo match", ""); err != nil {
		t.Fatalf("record stale-host job: %v", err)
	}
	if err := db.RecordHostSync(database, "fresh-host", time.Now()); err != nil {
		t.Fatalf("record fresh host sync: %v", err)
	}
	if err := db.RecordHostSync(database, "stale-host", time.Now().Add(-2*defaultHostSyncWindow)); err != nil {
		t.Fatalf("record stale host sync: %v", err)
	}

	listFormat, listAll, listLimit = "json", true, 0
	selection := requireJobListJSONSelection(t, renderJobListJSONDocument(t, database))
	constraints := requireJobListJSONConstraints(t, selection)
	if len(constraints) != 1 {
		t.Fatalf("selection.constraints = %v, want host sync window only", constraints)
	}
	constraint := requireJobListJSONConstraint(t, constraints, string(jobListJSONConstraintHostSyncWindow))
	if constraint["value"] != float64(defaultHostSyncWindow/time.Second) {
		t.Errorf("host_sync_window.value = %v, want %v", constraint["value"], defaultHostSyncWindow/time.Second)
	}
	if constraint["hidden"] != float64(1) {
		t.Errorf("host_sync_window.hidden = %v, want 1", constraint["hidden"])
	}
	if constraint["requested"] != false {
		t.Errorf("host_sync_window.requested = %v, want false", constraint["requested"])
	}
	if selection["complete"] != false {
		t.Errorf("selection.complete = %v, want false", selection["complete"])
	}
}

func TestJobListJSONSelectionDerivesCompleteFromAllConstraints(t *testing.T) {
	restoreListFlags(t)
	value := 7
	zero := 0
	nonzero := 1
	tests := []struct {
		name   string
		hidden *int
		want   bool
	}{
		{"zero", &zero, true},
		{"nonzero", &nonzero, false},
		{"unknown", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			selection := newJobListJSONSelection(jobListJSONOrderNewestFirst, []jobListJSONConstraint{{
				Kind: jobListJSONConstraintMaxAgeDays, Value: &value, Hidden: tc.hidden, Requested: true,
			}})
			if selection.Complete != tc.want {
				t.Errorf("Complete = %v, want %v", selection.Complete, tc.want)
			}
		})
	}
	unknown := newJobListJSONSelection(jobListJSONOrderNewestFirst, []jobListJSONConstraint{{
		Kind: jobListJSONConstraintMaxAgeDays, Value: &value, Hidden: nil, Requested: true,
	}})
	encoded, err := json.Marshal(unknown)
	if err != nil {
		t.Fatalf("marshal unknown hidden count: %v", err)
	}
	if !bytes.Contains(encoded, []byte(`"hidden":null`)) {
		t.Errorf("unknown hidden count encoded as %s, want hidden:null", encoded)
	}

	listMaxAgeDaysApplied = defaultListMaxAgeDays
	listOlderHiddenCount = 0
	listOlderHiddenCountKnown = false
	listLimit = 0
	listHostSyncWindowApplied = false
	selection := currentJobListJSONSelection()
	if len(selection.Constraints) != 1 || selection.Constraints[0].Hidden != nil {
		t.Fatalf("uncounted max-age constraint = %v, want hidden nil", selection.Constraints)
	}
	if selection.Complete {
		t.Fatal("uncounted max-age constraint produced complete=true")
	}
}

func TestJobListJSONSelectionRejectsValuesOutsideClosedVocabularies(t *testing.T) {
	if got, want := []string{
		string(jobListJSONConstraintMaxAgeDays),
		string(jobListJSONConstraintLimit),
		string(jobListJSONConstraintHostSyncWindow),
	}, []string{"max_age_days", "limit", "host_sync_window"}; !slices.Equal(got, want) {
		t.Fatalf("constraint vocabulary = %v, want %v", got, want)
	}
	if got, want := []string{
		string(jobListJSONOrderNewestFirst),
		string(jobListJSONOrderActiveFirst),
	}, []string{"newest_first", "active_first"}; !slices.Equal(got, want) {
		t.Fatalf("order vocabulary = %v, want %v", got, want)
	}

	value, hidden := 1, 0
	tests := []struct {
		name      string
		selection jobListJSONSelection
	}{
		{
			name: "constraint kind",
			selection: newJobListJSONSelection(jobListJSONOrderNewestFirst, []jobListJSONConstraint{{
				Kind: "future_kind", Value: &value, Hidden: &hidden, Requested: true,
			}}),
		},
		{
			name:      "order",
			selection: newJobListJSONSelection("database_order", nil),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := writeJobListJSON(&output, nil, nil, tc.selection); err == nil {
				t.Fatal("writeJobListJSON accepted a value outside the closed vocabulary")
			}
			if output.Len() != 0 {
				t.Errorf("invalid selection emitted %q", output.String())
			}
		})
	}
}

func TestJobListJSONSelectionReportsCollectorOrder(t *testing.T) {
	restoreListFlags(t)
	database := db.SetupTestDB(t)
	listAll, listAllHosts, listLimit = true, true, 0

	if _, err := collectJobsForList(database, nil); err != nil {
		t.Fatalf("collectJobsForList: %v", err)
	}
	if got := currentJobListJSONSelection().Order; got != jobListJSONOrderActiveFirst {
		t.Errorf("shared collector order = %q, want %q", got, jobListJSONOrderActiveFirst)
	}

	listNewestFirst = true
	if _, err := collectJobsForList(database, nil); err != nil {
		t.Fatalf("collectJobsForList newest-first: %v", err)
	}
	if got := currentJobListJSONSelection().Order; got != jobListJSONOrderNewestFirst {
		t.Errorf("plain-list collector order = %q, want %q", got, jobListJSONOrderNewestFirst)
	}
}

func TestJobListJSONSelectionOmitsEveryUnappliedConstraint(t *testing.T) {
	restoreListFlags(t)
	database := db.SetupTestDB(t)
	listAll, listAllHosts, listLimit = true, true, 0
	if _, err := collectJobsForList(database, nil); err != nil {
		t.Fatalf("collectJobsForList: %v", err)
	}
	selection := currentJobListJSONSelection()
	if len(selection.Constraints) != 0 {
		t.Fatalf("Constraints = %v, want empty", selection.Constraints)
	}
	if slices.ContainsFunc(selection.Constraints, func(c jobListJSONConstraint) bool { return c.Value == nil }) {
		t.Fatal("unapplied constraint was represented by a null value")
	}
}
