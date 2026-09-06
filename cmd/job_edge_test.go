package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

func seedJobDetailFixture(t *testing.T) (*db.Job, edgeViewDeps) {
	t.Helper()
	database := db.SetupTestDB(t)
	if err := db.RecordQueuedWithGPUAndID(database, 42, "", "/tmp/project", "uv run train.py", "fixture job", ""); err != nil {
		t.Fatal(err)
	}
	// A future fixture time suppresses the wall-clock-derived "Waiting" line
	// while keeping the job inside the published recency window.
	if _, err := database.Exec(`UPDATE jobs SET created_at = ? WHERE id = ?`, int64(4102444800), int64(42)); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, int64(4102444800), int64(42)); err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, 42)
	if err != nil || job == nil {
		t.Fatalf("GetJobByID: job=%v err=%v", job, err)
	}
	return job, edgeViewDeps{DB: database, Cfg: &config.Config{}}
}

func withoutUsageHints(t *testing.T) {
	t.Helper()
	hintsMu.Lock()
	savedConfigured, savedEnabled := hintsConfigured, hintsEnabled
	hintsConfigured, hintsEnabled = true, false
	hintsMu.Unlock()
	t.Cleanup(func() {
		hintsMu.Lock()
		hintsConfigured, hintsEnabled = savedConfigured, savedEnabled
		hintsMu.Unlock()
	})
}

// TestJobPresentationGolden pins the writer renderer's exact bytes. It kills
// mutations that add separators, trim the final newline, or rewrite labels.
func TestJobPresentationGolden(t *testing.T) {
	fixture := jobPresentationView{
		JobID:    "wj42",
		Text:     "Job ID:   wj42\nHost:     unplaced\nStatus:   queued\n",
		Warnings: "warning: telemetry snapshot unavailable\n",
	}
	var got, gotErr bytes.Buffer
	if err := renderJobPresentation(&got, &gotErr, fixture); err != nil {
		t.Fatal(err)
	}
	const want = "Job ID:   wj42\nHost:     unplaced\nStatus:   queued\n"
	if got.String() != want {
		t.Fatalf("rendered bytes = %q, want %q", got.String(), want)
	}
	if gotErr.String() != fixture.Warnings {
		t.Fatalf("rendered warnings = %q, want %q", gotErr.String(), fixture.Warnings)
	}
}

// TestJobDetailHubRenderMatchesModel verifies the ledger builder and hub text
// renderer use the same serializable model for both status and info. It kills
// a mutation that routes either hub command around the model renderer.
func TestJobDetailHubRenderMatchesModel(t *testing.T) {
	withoutUsageHints(t)
	_, deps := seedJobDetailFixture(t)
	view, err := buildJobDetailView(deps.DB, 42, false)
	if err != nil {
		t.Fatal(err)
	}
	if view.Status.Text == "" || view.Info.Text == "" {
		t.Fatalf("fixture model: status=%q info=%q", view.Status.Text, view.Info.Text)
	}
	for name, presentation := range map[string]jobPresentationView{"status": view.Status, "info": view.Info} {
		t.Run(name, func(t *testing.T) {
			var got bytes.Buffer
			if err := renderJobPresentation(&got, &got, presentation); err != nil {
				t.Fatal(err)
			}
			if got.String() != presentation.Text || !strings.HasSuffix(got.String(), "\n") {
				t.Fatalf("render = %q, model text = %q", got.String(), presentation.Text)
			}
		})
	}
}

// TestHubJobCommandsRenderThePublishedModel kills a mutation that leaves the
// ordinary hub command on a separate legacy formatter.
func TestHubJobCommandsRenderThePublishedModel(t *testing.T) {
	withoutUsageHints(t)
	_, deps := seedJobDetailFixture(t)
	view, err := buildJobDetailView(deps.DB, 42, false)
	if err != nil {
		t.Fatal(err)
	}
	savedMirror := activeEdgeMirror
	activeEdgeMirror = nil
	t.Cleanup(func() { activeEdgeMirror = savedMirror })

	setStatusEdgeFlags(t)
	var statusOut, statusErr bytes.Buffer
	statusCmd := &cobra.Command{}
	statusCmd.SetOut(&statusOut)
	statusCmd.SetErr(&statusErr)
	if err := runJobStatus(statusCmd, []string{"wj42"}); err != nil {
		t.Fatal(err)
	}
	if statusOut.String() != view.Status.Text {
		t.Fatalf("hub status = %q, model = %q", statusOut.String(), view.Status.Text)
	}

	setJobInfoEdgeFlags(t)
	var infoOut, infoErr bytes.Buffer
	infoCmd := &cobra.Command{}
	infoCmd.SetOut(&infoOut)
	infoCmd.SetErr(&infoErr)
	if err := runJobInfo(infoCmd, []string{"wj42"}); err != nil {
		t.Fatal(err)
	}
	if infoOut.String() != view.Info.Text {
		t.Fatalf("hub info = %q, model = %q", infoOut.String(), view.Info.Text)
	}
}

// TestJobDetailEdgeTextParity publishes through a filesystem transport and
// compares edge text to the hub model above the provenance line. It kills a
// mutation that selects the wrong submodel or renders the view independently.
func TestJobDetailEdgeTextParity(t *testing.T) {
	withoutUsageHints(t)
	_, deps := seedJobDetailFixture(t)
	view, err := buildJobDetailView(deps.DB, 42, false)
	if err != nil {
		t.Fatal(err)
	}
	em := edgeTestRuntime(t, deps)

	setStatusEdgeFlags(t)
	statusOut, _, err := captureEdgeCmd(t, "status", func(cmd *cobra.Command) error {
		return runJobStatusEdge(cmd, []string{"wj42"}, em)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(statusOut, view.Status.Text) || !strings.Contains(statusOut, "source: hub test-hub via ") {
		t.Fatalf("edge status = %q, hub model = %q", statusOut, view.Status.Text)
	}

	setJobInfoEdgeFlags(t)
	infoOut, _, err := captureEdgeCmd(t, "info", func(cmd *cobra.Command) error {
		return runJobInfoEdge(cmd, []string{"wj42"}, em)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(infoOut, view.Info.Text) || !strings.Contains(infoOut, "source: hub test-hub via ") {
		t.Fatalf("edge info = %q, hub model = %q", infoOut, view.Info.Text)
	}
}

func setStatusEdgeFlags(t *testing.T) {
	t.Helper()
	saved := []bool{statusSync, statusNoSync, statusFast, statusWait, statusJSON}
	savedTimeout := statusSSHTimeout
	t.Cleanup(func() {
		statusSync, statusNoSync, statusFast, statusWait, statusJSON = saved[0], saved[1], saved[2], saved[3], saved[4]
		statusSSHTimeout = savedTimeout
	})
	statusSync, statusNoSync, statusFast, statusWait, statusJSON = false, true, false, false, false
	statusSSHTimeout = 0
}

func setJobInfoEdgeFlags(t *testing.T) {
	t.Helper()
	saved := []bool{jobInfoSync, jobInfoNoSync, jobInfoAllAttempts, jobInfoJSON}
	t.Cleanup(func() {
		jobInfoSync, jobInfoNoSync, jobInfoAllAttempts, jobInfoJSON = saved[0], saved[1], saved[2], saved[3]
	})
	jobInfoSync, jobInfoNoSync, jobInfoAllAttempts, jobInfoJSON = false, true, false, false
}

// TestJobDetailProducerUsesFormattedSectionID kills a mutation that publishes
// a job document under numeric or mismatched identity.
func TestJobDetailProducerUsesFormattedSectionID(t *testing.T) {
	withoutUsageHints(t)
	_, deps := seedJobDetailFixture(t)
	body, err := produceJobDetailSection(context.Background(), deps, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"job_id": "wj42"`)) {
		t.Fatalf("job detail section = %s", body)
	}
}

// TestJobStatusJSONPinsStructuredFieldsAndEdgeSource kills mutations that
// omit the structured by-product or attach provenance outside the source key.
func TestJobStatusJSONPinsStructuredFieldsAndEdgeSource(t *testing.T) {
	withoutUsageHints(t)
	_, deps := seedJobDetailFixture(t)
	em := edgeTestRuntime(t, deps)
	setStatusEdgeFlags(t)
	statusJSON = true
	stdout, _, err := captureEdgeCmd(t, "status", func(cmd *cobra.Command) error {
		return runJobStatusEdge(cmd, []string{"wj42"}, em)
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Version int    `json:"version"`
		JobID   string `json:"job_id"`
		Host    string `json:"host"`
		Status  string `json:"status"`
		Source  struct {
			HubHost string `json:"hub_host"`
		} `json:"source"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || got.JobID != "wj42" || got.Host != "(unplaced)" || got.Status != "queued" || got.Source.HubHost != "test-hub" {
		t.Fatalf("status JSON = %+v", got)
	}
}
