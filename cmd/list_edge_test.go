package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/spf13/cobra"
)

// setListFlags pins the list flag globals a test needs and restores them
// after. The flags are package-level; without this one test's --project would
// leak into the next.
func setListFlags(t *testing.T, set func()) {
	t.Helper()
	savedFormat, savedProject, savedLimit := listFormat, listProject, listLimit
	savedColumns, savedNoTruncate := listColumns, listNoTruncate
	t.Cleanup(func() {
		listFormat, listProject, listLimit = savedFormat, savedProject, savedLimit
		listColumns, listNoTruncate = savedColumns, savedNoTruncate
	})
	set()
}

func seedListFixture(t *testing.T, database *sql.DB) {
	t.Helper()
	for _, project := range []string{"alpha", "beta"} {
		id, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "job")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetJobProject(database, id, project); err != nil {
			t.Fatal(err)
		}
	}
}

// TestJobListEdgeJSONParity: the edge's --format json is the hub's serializer
// run on the published model, plus the source key and nothing else.
func TestJobListEdgeJSONParity(t *testing.T) {
	database := db.SetupTestDB(t)
	seedListFixture(t, database)
	em := edgeTestRuntime(t, edgeViewDeps{DB: database})

	// The bare-list ordering is what renderListPlain sets for the hub's own
	// default listing; the producer publishes the same ordering.
	listNewestFirst = true
	model, err := buildJobListModel(database, nil)
	listNewestFirst = false
	if err != nil {
		t.Fatal(err)
	}
	cols, err := terminal.ResolveColumns(nil, terminal.DefaultJSONColumnKeys)
	if err != nil {
		t.Fatal(err)
	}
	var hub bytes.Buffer
	if err := writeJobListJSON(&hub, model.Jobs, cols, model.Selection, nil); err != nil {
		t.Fatal(err)
	}

	setListFlags(t, func() { listFormat = "json" })
	stdout, _, err := captureEdgeCmd(t, "list", func(cmd *cobra.Command) error {
		return runListEdge(cmd, nil, em)
	})
	if err != nil {
		t.Fatal(err)
	}

	var edgeDoc, hubDoc map[string]any
	if err := json.Unmarshal([]byte(stdout), &edgeDoc); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(hub.Bytes(), &hubDoc); err != nil {
		t.Fatal(err)
	}
	source, ok := edgeDoc["source"].(map[string]any)
	if !ok || source["hub_host"] != "test-hub" {
		t.Fatalf("edge JSON source = %v", edgeDoc["source"])
	}
	delete(edgeDoc, "source")
	edgeBytes, _ := json.Marshal(edgeDoc)
	hubBytes, _ := json.Marshal(hubDoc)
	if !bytes.Equal(edgeBytes, hubBytes) {
		t.Fatalf("edge JSON diverges from hub JSON:\nedge: %s\nhub: %s", edgeBytes, hubBytes)
	}
	if len(hubDoc["jobs"].([]any)) != 2 {
		t.Fatalf("jobs = %v", hubDoc["jobs"])
	}
}

func TestBuildJobListModelCarriesKillAttribution(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "job")
	if err != nil {
		t.Fatal(err)
	}
	const killedAt = int64(1_700_000_000)
	if err := db.SetKillRequestedStatus(database, jobID, "session:edge-killer", "blocked queue", killedAt); err != nil {
		t.Fatal(err)
	}

	model, err := buildJobListModel(database, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(model.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(model.Jobs))
	}
	job := model.Jobs[0]
	if job.KillActor != "session:edge-killer" || job.KillReason != "blocked queue" ||
		job.KilledAt == nil || *job.KilledAt != killedAt {
		t.Fatalf("published kill attribution = actor %q reason %q at %v", job.KillActor, job.KillReason, job.KilledAt)
	}
}

// TestJobListEdgeTableParity: the edge's table is the hub's renderer run on
// the published model, byte-equal up to the provenance line.
func TestJobListEdgeTableParity(t *testing.T) {
	database := db.SetupTestDB(t)
	seedListFixture(t, database)
	em := edgeTestRuntime(t, edgeViewDeps{DB: database})

	listNewestFirst = true
	model, err := buildJobListModel(database, nil)
	listNewestFirst = false
	if err != nil {
		t.Fatal(err)
	}
	hub := terminal.RenderJobListPlainWithOptions(model.Jobs, terminal.ListOutputWidth(), nil, false)

	setListFlags(t, func() { listFormat = "table" })
	stdout, _, err := captureEdgeCmd(t, "list", func(cmd *cobra.Command) error {
		return runListEdge(cmd, nil, em)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, hub) {
		t.Fatalf("edge table diverges from hub:\nedge: %q\nhub: %q", stdout, hub)
	}
	if !strings.Contains(stdout, "source: hub test-hub via ") {
		t.Fatalf("no provenance line in %q", stdout)
	}
}

// TestJobListEdgeNarrowsByProject: a pure narrowing filter runs on the edge
// against the published rows.
func TestJobListEdgeNarrowsByProject(t *testing.T) {
	database := db.SetupTestDB(t)
	seedListFixture(t, database)
	em := edgeTestRuntime(t, edgeViewDeps{DB: database})

	setListFlags(t, func() {
		listFormat = "json"
		listProject = "alpha"
	})
	stdout, _, err := captureEdgeCmd(t, "list", func(cmd *cobra.Command) error {
		return runListEdge(cmd, nil, em)
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatal(err)
	}
	jobs := doc["jobs"].([]any)
	if len(jobs) != 1 || jobs[0].(map[string]any)["project"] != "alpha" {
		t.Fatalf("jobs = %v", jobs)
	}
	// The narrowed selection must not borrow the unfiltered listing's hidden
	// counts: it reports complete=false rather than a number nobody computed.
	selection := doc["selection"].(map[string]any)
	if selection["complete"] != false {
		t.Fatalf("narrowed selection claims completeness: %v", selection)
	}
}

// TestCheckListEdgeFlags pins each refusal to its named bound.
func TestCheckListEdgeFlags(t *testing.T) {
	cases := []struct {
		name    string
		set     func()
		args    []string
		wantErr string
	}{
		{"widening --all", func() { listAll = true }, nil, "--all"},
		{"widening --since", func() { listSince = "30d" }, nil, "--since"},
		{"widening --limit", func() { listLimit = 500 }, nil, "--limit 500"},
		{"search", func() { listSearch = "train" }, nil, "search"},
		{"group-by", func() { listGroupBy = "status" }, nil, "--group-by"},
		{"tui", func() { listTUI = true }, nil, "interactive"},
		{"job ids", func() {}, []string{"wj1"}, "status <id>"},
		{"cleanup", func() { listCleanup = 3 }, nil, "disabled on an edge"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			saved := []bool{listAll, listTUI, listWatch}
			savedStr := []string{listSince, listSearch, listGroupBy}
			savedLimit, savedCleanup := listLimit, listCleanup
			defer func() {
				listAll, listTUI, listWatch = saved[0], saved[1], saved[2]
				listSince, listSearch, listGroupBy = savedStr[0], savedStr[1], savedStr[2]
				listLimit, listCleanup = savedLimit, savedCleanup
			}()
			tc.set()
			err := checkListEdgeFlags(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to name %q", err, tc.wantErr)
			}
		})
	}
	// The default flag set is servable.
	if err := checkListEdgeFlags(nil); err != nil {
		t.Fatalf("default flags refused: %v", err)
	}
}

// TestJobListEdgeFreshEmpty: a fresh, empty index is a true empty and renders
// the hub's empty listing, never a blocked outcome.
func TestJobListEdgeFreshEmpty(t *testing.T) {
	database := db.SetupTestDB(t)
	em := edgeTestRuntime(t, edgeViewDeps{DB: database})

	setListFlags(t, func() { listFormat = "table" })
	stdout, _, err := captureEdgeCmd(t, "list", func(cmd *cobra.Command) error {
		return runListEdge(cmd, nil, em)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "source: hub test-hub via ") {
		t.Fatalf("no provenance line in %q", stdout)
	}
}
