package cmd

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
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
					{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/project-alpha", Project: "EXP-ALPHA", Description: "train model"},
				},
			},
		},
		jobProgressHWM: map[int64]int{},
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "EXP-ALPHA") {
		t.Fatalf("output missing job project, got:\n%s", out)
	}
}

func TestWatchModelFinalRefreshUsesTerminalDBStateBeforeQuit(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateCloudInstance(database, &db.CloudInstance{
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateCloudInstance: %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO jobs (id, cloud_instance_id, host, tombstoned, status, command, working_dir, description)
		 VALUES (199, ?, ?, 0, ?, 'uv run llm-perf run exp_035_memory_capacity_cliff', '/workspace/llm-performance-models', 'EXP-035')`,
		instanceID, db.CloudInstanceHost(instanceID), db.StatusRunning,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if err := db.InsertJobCloudAttempt(database, 199, instanceID); err != nil {
		t.Fatalf("InsertJobCloudAttempt: %v", err)
	}

	m := watchModel{
		instanceIDs: []int64{instanceID},
		updates: map[int64]campaign.InstanceUpdate{
			instanceID: {
				CloudInstance: &db.CloudInstance{
					ID:       instanceID,
					Status:   db.CloudInstanceStatusRunning,
					Provider: "vastai",
					GPUSpec:  "A40",
				},
				Instance: &cloud.Instance{Status: "running"},
				Jobs: []*db.Job{
					{
						ID:              199,
						Status:          db.StatusRunning,
						CloudInstanceID: &instanceID,
						Host:            db.CloudInstanceHost(instanceID),
						WorkingDir:      "/workspace/llm-performance-models",
						Description:     "EXP-035",
					},
				},
			},
		},
		database:       database,
		jobProgressHWM: map[int64]int{},
	}

	if err := db.UpdateCloudInstanceStatus(database, instanceID, db.CloudInstanceStatusFailed, db.TerminationReasonDiskFull); err != nil {
		t.Fatalf("UpdateCloudInstanceStatus: %v", err)
	}
	if _, err := db.ResetCloudInstanceJobs(database, instanceID, db.AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("ResetCloudInstanceJobs: %v", err)
	}

	nextModel, cmd := m.Update(watchCheckDoneResultMsg{allTerminal: true})
	refreshMsg := cmd()
	refreshedModel, _ := nextModel.(watchModel).Update(refreshMsg)
	got := refreshedModel.(watchModel)

	if !got.done {
		t.Fatalf("expected done=true after final refresh")
	}
	if got.updates[instanceID].CloudInstance == nil || got.updates[instanceID].CloudInstance.Status != db.CloudInstanceStatusFailed {
		t.Fatalf("instance status = %+v, want failed", got.updates[instanceID].CloudInstance)
	}
	if got.updates[instanceID].JobAttemptOutcomes[199] != db.AttemptOutcomeOrphaned {
		t.Fatalf("job outcome = %q, want %q", got.updates[instanceID].JobAttemptOutcomes[199], db.AttemptOutcomeOrphaned)
	}

	out := stripANSI(got.View())
	if !strings.Contains(out, "failed (disk_full)") {
		t.Fatalf("expected failed instance in view, got:\n%s", out)
	}
	if !strings.Contains(out, "orphaned") {
		t.Fatalf("expected orphaned job in view, got:\n%s", out)
	}
}

func stripANSI(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAllString(s, "")
}
