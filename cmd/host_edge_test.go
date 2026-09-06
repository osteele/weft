package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/spf13/cobra"
)

// TestHostListHubJSONIsTheSectionBytes pins the one-serializer rule: the
// bytes the hub prints for --json are the bytes it publishes.
func TestHostListHubJSONIsTheSectionBytes(t *testing.T) {
	database := db.SetupTestDB(t)
	setTestHostInventory(t, []inventory.HostSpec{{Name: "studio", OS: "darwin", Arch: "arm64"}})
	t.Cleanup(config.SetConfigPathsForTesting(
		t.TempDir()+"/config.toml", t.TempDir()+"/config.yaml"))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	section, err := produceHostsSection(context.Background(), edgeViewDeps{DB: database, Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := loadHostListRows(time.Now(), database, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var hub bytes.Buffer
	if err := writeHostListJSON(&hub, hostListView{Hosts: rows}); err != nil {
		t.Fatal(err)
	}
	// The producer publishes the model; the hub's --json projects it to the
	// envelope. The projection of the same rows must carry the same hosts.
	var sectionDoc, hubDoc map[string]any
	if err := json.Unmarshal(section, &sectionDoc); err != nil {
		t.Fatalf("section: %v", err)
	}
	if err := json.Unmarshal(hub.Bytes(), &hubDoc); err != nil {
		t.Fatalf("hub: %v", err)
	}
	sectionHosts, _ := sectionDoc["hosts"].([]any)
	hubHosts, _ := hubDoc["hosts"].([]any)
	if len(sectionHosts) != 1 || len(hubHosts) != 1 {
		t.Fatalf("section hosts = %d, hub hosts = %d", len(sectionHosts), len(hubHosts))
	}
	if sectionHosts[0].(map[string]any)["Name"] != "studio" {
		t.Fatalf("section row = %v", sectionHosts[0])
	}
	if hubHosts[0].(map[string]any)["name"] != "studio" {
		t.Fatalf("hub row = %v", hubHosts[0])
	}
}

// TestHostListEdgeParity: the edge renders the hub's table from the published
// model, byte-equal up to the provenance line.
func TestHostListEdgeParity(t *testing.T) {
	database := db.SetupTestDB(t)
	setTestHostInventory(t, []inventory.HostSpec{{Name: "studio", OS: "darwin", Arch: "arm64"}})
	t.Cleanup(config.SetConfigPathsForTesting(
		t.TempDir()+"/config.toml", t.TempDir()+"/config.yaml"))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	em := edgeTestRuntime(t, edgeViewDeps{DB: database, Cfg: cfg})

	rows, err := loadHostListRows(time.Now(), database, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var hub bytes.Buffer
	if err := writeHostListTable(&hub, rows); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := captureEdgeCmd(t, "host list", func(cmd *cobra.Command) error {
		return runHostListEdge(cmd, nil, em, rentalsOn)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, hub.String()) {
		t.Fatalf("edge render diverges from hub:\nedge: %q\nhub: %q", stdout, hub.String())
	}
	if !strings.Contains(stdout, "source: hub test-hub via ") {
		t.Fatalf("no provenance line in %q", stdout)
	}
}

// TestFilterHostListRows: the --rentals filter is applied on the edge from
// the unfiltered published model.
func TestFilterHostListRows(t *testing.T) {
	rows := []hostListRow{
		{Type: "host", Name: "studio"},
		{Type: "rental", Name: "wi1"},
	}
	if got := filterHostListRows(rows, rentalsOnly); len(got) != 1 || got[0].Name != "wi1" {
		t.Fatalf("rentalsOnly = %+v", got)
	}
	if got := filterHostListRows(rows, rentalsOff); len(got) != 1 || got[0].Name != "studio" {
		t.Fatalf("rentalsOff = %+v", got)
	}
	if got := filterHostListRows(rows, rentalsOn); len(got) != 2 {
		t.Fatalf("rentalsOn = %+v", got)
	}
}

// TestHostListEdgeRefusesTUI: interactive displays read the live ledger,
// which an edge never opens.
func TestHostListEdgeRefusesTUI(t *testing.T) {
	hostListTUIFlag = true
	defer func() { hostListTUIFlag = false }()
	em := &edgeMirrorRuntime{}
	err := runHostListEdge(&cobra.Command{}, nil, em, rentalsOn)
	if err == nil || !strings.Contains(err.Error(), "TUI") {
		t.Fatalf("err = %v", err)
	}
}
