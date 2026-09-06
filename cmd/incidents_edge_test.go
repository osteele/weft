package cmd

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/edge"
	"github.com/osteele/weft/internal/edgeview"
	"github.com/spf13/cobra"
)

// seedIncidentFixture writes two unplaced jobs sharing a fingerprint, so one
// incident exists.
func seedIncidentFixture(t *testing.T, database *sql.DB) {
	t.Helper()
	for i := 0; i < 2; i++ {
		id, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "job")
		if err != nil {
			t.Fatal(err)
		}
		s := &blockreason.Structured{
			Summary:     "planner: search offers gpu_ram>=10 …",
			Fingerprint: "vastai/search-offers/400/bad-field:driver_vers",
		}
		if err := db.SetJobPlacementBlocked(database, id, s.Marshal()); err != nil {
			t.Fatal(err)
		}
	}
}

// edgeTestRuntime publishes the registered producers into a filesystem
// transport and returns the reader-side runtime an edge command would get.
func edgeTestRuntime(t *testing.T, deps edgeViewDeps) *edgeMirrorRuntime {
	t.Helper()
	transport, err := edge.NewFSTransport(filepath.Join(t.TempDir(), "view"))
	if err != nil {
		t.Fatal(err)
	}
	publisher := edgeview.NewPublisher(transport, "test-hub", "test")
	sections := edgeViewProducers(context.Background(), deps)
	if _, err := publisher.Publish(context.Background(), sections); err != nil {
		t.Fatal(err)
	}
	return &edgeMirrorRuntime{
		reader:     edgeview.NewReader(transport),
		staleAfter: 5 * time.Minute,
	}
}

// captureEdgeCmd runs fn against the real command at path (so flag lookups
// behave as they do in production) with its streams captured.
func captureEdgeCmd(t *testing.T, path string, fn func(cmd *cobra.Command) error) (stdout, stderr string, err error) {
	t.Helper()
	cmd := findCmd(t, path)
	cmd.SetContext(context.Background())
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	defer func() {
		cmd.SetOut(nil)
		cmd.SetErr(nil)
	}()
	err = fn(cmd)
	return outBuf.String(), errBuf.String(), err
}

// TestIncidentsHubJSONIsTheSectionBytes pins the one-serializer rule: the
// bytes the hub prints for --json are the bytes it publishes.
func TestIncidentsHubJSONIsTheSectionBytes(t *testing.T) {
	database := db.SetupTestDB(t)
	seedIncidentFixture(t, database)

	section, err := produceIncidentsSection(context.Background(), edgeViewDeps{DB: database})
	if err != nil {
		t.Fatal(err)
	}
	incs, err := collectIncidents(database)
	if err != nil {
		t.Fatal(err)
	}
	var hub bytes.Buffer
	if err := writeIncidentsJSON(&hub, incidentsView{Incidents: incs}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(section, hub.Bytes()) {
		t.Fatalf("section bytes diverge from hub --json:\nsection: %s\nhub: %s", section, hub.Bytes())
	}
}

// TestIncidentsEdgeParity renders the same model on hub and edge and requires
// equality up to the provenance line.
func TestIncidentsEdgeParity(t *testing.T) {
	database := db.SetupTestDB(t)
	seedIncidentFixture(t, database)
	em := edgeTestRuntime(t, edgeViewDeps{DB: database})

	var hub bytes.Buffer
	incs, err := collectIncidents(database)
	if err != nil {
		t.Fatal(err)
	}
	if err := renderIncidents(&hub, incidentsView{Incidents: incs}, false); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := captureEdgeCmd(t, "incidents", func(cmd *cobra.Command) error {
		return runIncidentsEdge(cmd, nil, em)
	})
	if err != nil {
		t.Fatal(err)
	}
	provLine := stdout[strings.LastIndex(stdout, "source: hub "):]
	if !strings.HasPrefix(provLine, "source: hub test-hub via ") {
		t.Fatalf("no provenance line in %q", stdout)
	}
	if got := strings.TrimSuffix(stdout[:strings.LastIndex(stdout, "source: hub ")], ""); got != hub.String() {
		t.Fatalf("edge render diverges from hub render:\nedge: %q\nhub: %q", got, hub.String())
	}
}

// TestIncidentsEdgeJSONCarriesSource: the edge's --json adds the source key;
// the hub's never carries it.
func TestIncidentsEdgeJSONCarriesSource(t *testing.T) {
	database := db.SetupTestDB(t)
	seedIncidentFixture(t, database)
	em := edgeTestRuntime(t, edgeViewDeps{DB: database})

	incidentsJSON = true
	defer func() { incidentsJSON = false }()
	stdout, _, err := captureEdgeCmd(t, "incidents", func(cmd *cobra.Command) error {
		return runIncidentsEdge(cmd, nil, em)
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stdout), &decoded); err != nil {
		t.Fatal(err)
	}
	source, ok := decoded["source"].(map[string]any)
	if !ok {
		t.Fatalf("no source key in %s", stdout)
	}
	if source["hub_host"] != "test-hub" {
		t.Fatalf("source.hub_host = %v", source["hub_host"])
	}
}

// TestIncidentsEdgeBlockedWithoutManifest: an edge whose hub never published
// gets the blocked outcome and no rendered table.
func TestIncidentsEdgeBlockedWithoutManifest(t *testing.T) {
	transport, err := edge.NewFSTransport(filepath.Join(t.TempDir(), "view"))
	if err != nil {
		t.Fatal(err)
	}
	em := &edgeMirrorRuntime{reader: edgeview.NewReader(transport), staleAfter: 5 * time.Minute}
	stdout, stderr, err := captureEdgeCmd(t, "incidents", func(cmd *cobra.Command) error {
		return runIncidentsEdge(cmd, nil, em)
	})
	var gateErr *EdgeGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("err = %v, want *EdgeGateError", err)
	}
	if gateErr.ExitCode != edgeExitBlocked {
		t.Fatalf("exit code = %d, want %d", gateErr.ExitCode, edgeExitBlocked)
	}
	if stdout != "" {
		t.Fatalf("blocked outcome printed %q", stdout)
	}
	if !strings.Contains(stderr, "hub not reachable from this edge: no hub view has been published") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// TestIncidentsEdgeFreshEmptyRenders: a fresh, empty incidents section is a
// true empty and renders exactly the hub's empty listing.
func TestIncidentsEdgeFreshEmptyRenders(t *testing.T) {
	database := db.SetupTestDB(t)
	em := edgeTestRuntime(t, edgeViewDeps{DB: database})
	stdout, _, err := captureEdgeCmd(t, "incidents", func(cmd *cobra.Command) error {
		return runIncidentsEdge(cmd, nil, em)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, "No active placement incidents.\n") {
		t.Fatalf("stdout = %q", stdout)
	}
}
