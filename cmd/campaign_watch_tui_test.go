package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/db"
)

func TestWatchModelView_PreUpdateUsesDBStatusAndTerminalSpinnerBehavior(t *testing.T) {
	database := db.SetupTestDB(t)

	failedID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusLaunching,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("create failed instance: %v", err)
	}
	if err := db.UpdateCloudInstanceStatus(database, failedID, db.CloudInstanceStatusFailed, db.TerminationReasonInfraFailure); err != nil {
		t.Fatalf("set failed status: %v", err)
	}

	launchingID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusLaunching,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("create launching instance: %v", err)
	}

	m := newWatchModel(database, []int64{failedID, launchingID}, nil)
	m.spinner.Spinner = spinner.Spinner{Frames: []string{"SPIN"}, FPS: time.Second}
	spinnerMarker := m.spinner.View()
	if spinnerMarker == "" {
		t.Fatal("spinner marker unexpectedly empty")
	}

	out := m.View()
	cleanOut := stripANSI(out)
	cleanSpinner := stripANSI(spinnerMarker)

	if !strings.Contains(cleanOut, "failed (infra_failure)") {
		t.Fatalf("output missing failed status label, got:\n%s", out)
	}
	if !strings.Contains(cleanOut, fmt.Sprintf("Instance %d — A100 — launching", launchingID)) {
		t.Fatalf("output missing launching status label, got:\n%s", out)
	}
	if !strings.Contains(cleanOut, "  vastai:") {
		t.Fatalf("output missing failed instance identity line, got:\n%s", out)
	}
	if !strings.Contains(cleanOut, "  vastai:") {
		t.Fatalf("output missing launching instance identity line, got:\n%s", out)
	}
	if strings.Contains(cleanOut, fmt.Sprintf("ID %d  vastai:", failedID)) || strings.Contains(cleanOut, fmt.Sprintf("ID %d  vastai:", launchingID)) {
		t.Fatalf("output should omit redundant provider line IDs, got:\n%s", out)
	}

	parts := strings.Split(cleanOut, fmt.Sprintf("Instance %d —", failedID))
	if len(parts) < 2 {
		t.Fatalf("could not isolate failed instance section, got:\n%s", out)
	}
	failedSection := strings.Split(parts[1], "\n\n")[0]
	if strings.Contains(failedSection, cleanSpinner) {
		t.Fatalf("failed instance section should not include spinner %q, section:\n%s", cleanSpinner, failedSection)
	}

	launchParts := strings.Split(cleanOut, fmt.Sprintf("Instance %d —", launchingID))
	if len(launchParts) < 2 {
		t.Fatalf("could not isolate launching instance section, got:\n%s", out)
	}
	launchingSection := strings.Split(launchParts[1], "\n\n")[0]
	if !strings.Contains(launchingSection, cleanSpinner) {
		t.Fatalf("launching instance section should include spinner %q, section:\n%s", cleanSpinner, launchingSection)
	}
}

func TestFormatWatchProviderLine(t *testing.T) {
	ci := &db.CloudInstance{
		ID:                 106,
		Status:             db.CloudInstanceStatusRunning,
		Provider:           "vastai",
		ProviderInstanceID: "32712486",
	}

	line := formatWatchProviderLine(ci, nil)
	if line != "  vastai: 32712486" {
		t.Fatalf("provider line = %q", line)
	}
}

func TestRenderWatchExitSnapshot(t *testing.T) {
	doneModel := watchModel{
		done:       true,
		campaignID: 48,
		launchedAt: time.Unix(1, 0),
	}
	if snapshot := renderWatchExitSnapshot(doneModel); snapshot == "" {
		t.Fatal("expected final snapshot for completed watch")
	}

	runningModel := watchModel{}
	if snapshot := renderWatchExitSnapshot(runningModel); snapshot != "" {
		t.Fatalf("unexpected snapshot for incomplete watch: %q", snapshot)
	}
}

func TestWatchModelView_ShowsJobDirectoryTail(t *testing.T) {
	m := watchModel{
		instanceIDs: []int64{5},
		updates: map[int64]campaign.InstanceUpdate{
			5: {
				CloudInstance: &db.CloudInstance{
					ID:       5,
					Status:   db.CloudInstanceStatusRunning,
					Provider: "vastai",
					GPUSpec:  "A100",
				},
				Jobs: []*db.Job{
					{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/project-alpha", Description: "train model"},
				},
			},
		},
		jobProgressHWM: map[int64]int{},
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "project-alpha") {
		t.Fatalf("output missing job directory tail, got:\n%s", out)
	}
}

func stripANSI(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAllString(s, "")
}
