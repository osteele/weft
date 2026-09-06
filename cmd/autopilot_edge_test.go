package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

// TestAutopilotHubJSONIsTheSectionBytes pins the one-serializer rule: the
// bytes the hub prints for --json are the bytes it publishes.
func TestAutopilotHubJSONIsTheSectionBytes(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.PauseAutopilot(database, "test", "fixture"); err != nil {
		t.Fatal(err)
	}

	section, err := produceAutopilotSection(context.Background(), edgeViewDeps{DB: database})
	if err != nil {
		t.Fatal(err)
	}
	view, err := buildAutopilotStatusModel(database)
	if err != nil {
		t.Fatal(err)
	}
	var hub bytes.Buffer
	if err := writeAutopilotStatusJSON(&hub, view); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(section, hub.Bytes()) {
		t.Fatalf("section bytes diverge from hub --json:\nsection: %s\nhub: %s", section, hub.Bytes())
	}
	if bytes.Contains(section, []byte(`"source"`)) {
		t.Fatal("hub JSON carries a source key")
	}
}

// TestAutopilotEdgeParity: hub text render and edge render of the published
// model agree up to the provenance line.
func TestAutopilotEdgeParity(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.PauseAutopilot(database, "test", "fixture"); err != nil {
		t.Fatal(err)
	}
	em := edgeTestRuntime(t, edgeViewDeps{DB: database})

	view, err := buildAutopilotStatusModel(database)
	if err != nil {
		t.Fatal(err)
	}
	hub := formatAutopilotStatusText(view) + "\n"

	stdout, _, err := captureEdgeCmd(t, "autopilot status", func(cmd *cobra.Command) error {
		return runAutopilotStatusEdge(cmd, nil, em)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stdout, hub) {
		t.Fatalf("edge render diverges from hub:\nedge: %q\nhub: %q", stdout, hub)
	}
	if !strings.Contains(stdout, "source: hub test-hub via ") {
		t.Fatalf("no provenance line in %q", stdout)
	}
}

// TestAutopilotEdgeJSONCarriesSource: the edge's --json adds the source key
// with the hub's identity.
func TestAutopilotEdgeJSONCarriesSource(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.PauseAutopilot(database, "test", "fixture"); err != nil {
		t.Fatal(err)
	}
	em := edgeTestRuntime(t, edgeViewDeps{DB: database})

	autopilotStatusJSON = true
	defer func() { autopilotStatusJSON = false }()
	stdout, _, err := captureEdgeCmd(t, "autopilot status", func(cmd *cobra.Command) error {
		return runAutopilotStatusEdge(cmd, nil, em)
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
	if decoded["state"] != "paused" {
		t.Fatalf("state = %v, want paused", decoded["state"])
	}
}
