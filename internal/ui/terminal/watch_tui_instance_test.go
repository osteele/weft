package terminal

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
	"github.com/osteele/weft/internal/ids"
)

func TestWatchModelView_PreUpdateUsesDBStatusAndTerminalSpinnerBehavior(t *testing.T) {
	database := db.SetupTestDB(t)

	failedID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusLaunching,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("create failed instance: %v", err)
	}
	if err := db.UpdateLaunchStatus(database, failedID, db.LaunchStatusFailed, db.TerminationReasonInfraFailure); err != nil {
		t.Fatalf("set failed status: %v", err)
	}

	launchingID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusLaunching,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("create launching instance: %v", err)
	}

	m := newCampaignWatchModel(database, []int64{failedID, launchingID}, nil, nil)
	defer m.cancel()
	m.spinner.Spinner = spinner.Spinner{Frames: []string{"SPIN"}, FPS: time.Second}
	spinnerMarker := m.spinner.View()
	if spinnerMarker == "" {
		t.Fatal("spinner marker unexpectedly empty")
	}

	out := m.View()
	cleanOut := stripANSI(out)
	cleanSpinner := stripANSI(spinnerMarker)

	if !strings.Contains(cleanOut, "terminated (infrastructure failure)") {
		t.Fatalf("output missing failed status label, got:\n%s", out)
	}
	if !strings.Contains(cleanOut, fmt.Sprintf("Instance %s — A100 — launching", ids.FormatInstanceID(launchingID))) {
		t.Fatalf("output missing launching status label, got:\n%s", out)
	}
	if !strings.Contains(cleanOut, "  vastai:") {
		t.Fatalf("output missing failed instance identity line, got:\n%s", out)
	}
	if !strings.Contains(cleanOut, "  vastai:") {
		t.Fatalf("output missing launching instance identity line, got:\n%s", out)
	}
	if strings.Contains(cleanOut, fmt.Sprintf("ID %s  vastai:", ids.FormatInstanceID(failedID))) || strings.Contains(cleanOut, fmt.Sprintf("ID %s  vastai:", ids.FormatInstanceID(launchingID))) {
		t.Fatalf("output should omit redundant provider line IDs, got:\n%s", out)
	}

	parts := strings.Split(cleanOut, fmt.Sprintf("Instance %s —", ids.FormatInstanceID(failedID)))
	if len(parts) < 2 {
		t.Fatalf("could not isolate failed instance section, got:\n%s", out)
	}
	failedSection := strings.Split(parts[1], "\n\n")[0]
	if strings.Contains(failedSection, cleanSpinner) {
		t.Fatalf("failed instance section should not include spinner %q, section:\n%s", cleanSpinner, failedSection)
	}

	launchParts := strings.Split(cleanOut, fmt.Sprintf("Instance %s —", ids.FormatInstanceID(launchingID)))
	if len(launchParts) < 2 {
		t.Fatalf("could not isolate launching instance section, got:\n%s", out)
	}
	launchingSection := strings.Split(launchParts[1], "\n\n")[0]
	if !strings.Contains(launchingSection, cleanSpinner) {
		t.Fatalf("launching instance section should include spinner %q, section:\n%s", cleanSpinner, launchingSection)
	}
}

func TestFormatWatchProviderLine(t *testing.T) {
	ci := &db.Launch{
		ID:                 106,
		Status:             db.LaunchStatusRunning,
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
				Launch: &db.Launch{
					ID:       5,
					Status:   db.LaunchStatusRunning,
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

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A40",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	if _, err := database.Exec(
		`INSERT INTO jobs (id, tombstoned, command, working_dir, description) VALUES (199, 0, 'uv run llm-perf run exp_035_memory_capacity_cliff', '/workspace/llm-performance-models', 'EXP-035')`,
	); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	db.CreateAttempt(database, 199, "", &instanceID, db.StatusRunning)
	database.Exec(`UPDATE job_attempts SET start_time = ? WHERE job_id = 199 AND end_time IS NULL`, time.Now().Unix())

	m := watchModel{
		instanceIDs: []int64{instanceID},
		updates: map[int64]campaign.InstanceUpdate{
			instanceID: {
				Launch: &db.Launch{
					ID:       instanceID,
					Status:   db.LaunchStatusRunning,
					Provider: "vastai",
					GPUSpec:  "A40",
				},
				Instance: &cloud.Instance{Status: cloud.ProviderStatusRunning},
				Jobs: []*db.Job{
					{
						ID:          199,
						Status:      db.StatusRunning,
						LaunchID:    &instanceID,
						Host:        db.LaunchHost(instanceID),
						WorkingDir:  "/workspace/llm-performance-models",
						Description: "EXP-035",
					},
				},
			},
		},
		database:       database,
		jobProgressHWM: map[int64]int{},
	}

	if err := db.UpdateLaunchStatus(database, instanceID, db.LaunchStatusFailed, db.TerminationReasonDiskFull); err != nil {
		t.Fatalf("UpdateLaunchStatus: %v", err)
	}
	if _, err := db.ResetLaunchJobs(database, instanceID, db.AttemptOutcomeOrphaned); err != nil {
		t.Fatalf("ResetLaunchJobs: %v", err)
	}

	nextModel, cmd := m.Update(watchCheckDoneResultMsg{allTerminal: true})
	refreshMsg := cmd()
	refreshedModel, _ := nextModel.(watchModel).Update(refreshMsg)
	got := refreshedModel.(watchModel)

	if !got.done {
		t.Fatalf("expected done=true after final refresh")
	}
	if got.updates[instanceID].Launch == nil || got.updates[instanceID].Launch.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %+v, want failed", got.updates[instanceID].Launch)
	}
	if got.updates[instanceID].JobAttemptOutcomes[199] != db.AttemptOutcomeOrphaned {
		t.Fatalf("job outcome = %q, want %q", got.updates[instanceID].JobAttemptOutcomes[199], db.AttemptOutcomeOrphaned)
	}

	out := stripANSI(got.View())
	if !strings.Contains(out, "terminated (disk full)") {
		t.Fatalf("expected failed instance in view, got:\n%s", out)
	}
	if !strings.Contains(out, "orphaned") {
		t.Fatalf("expected orphaned job in view, got:\n%s", out)
	}
	if strings.Contains(out, historicalLaunchJobsHeader) {
		t.Fatalf("expected failed instance to keep last attached jobs in the main list, got:\n%s", out)
	}
}

func TestWatchModelTerminalUpdateKeepsJobsInlineInRunOrder(t *testing.T) {
	instanceID := int64(130)
	m := watchModel{
		instanceIDs: []int64{instanceID},
		updates: map[int64]campaign.InstanceUpdate{
			instanceID: {
				Launch: &db.Launch{
					ID:       instanceID,
					Status:   db.LaunchStatusRunning,
					Provider: "vastai",
					GPUSpec:  "A40",
				},
				Jobs: []*db.Job{
					{ID: 203, Status: db.StatusRunning, LaunchID: &instanceID, Description: "current"},
					{ID: 249, Status: db.StatusQueued, Description: "historical"},
				},
				JobAttemptOutcomes: map[int64]string{
					249: db.AttemptOutcomeFailed,
				},
			},
		},
		jobProgressHWM: map[int64]int{},
	}

	terminalUpdate := campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:                instanceID,
			Status:            db.LaunchStatusFailed,
			Provider:          "vastai",
			GPUSpec:           "A40",
			TerminationReason: db.TerminationReasonInfraFailure,
		},
		Jobs: []*db.Job{
			{ID: 203, Status: db.StatusQueued, Description: "current"},
			{ID: 249, Status: db.StatusQueued, Description: "historical"},
		},
		JobAttemptOutcomes: map[int64]string{
			203: db.AttemptOutcomeOrphaned,
			249: db.AttemptOutcomeFailed,
		},
	}

	nextModel, cmd := m.Update(watchUpdateMsg{instanceID: instanceID, update: terminalUpdate})
	if cmd == nil {
		t.Fatal("expected follow-up wait command after terminal update")
	}

	out := stripANSI(nextModel.(watchModel).View())
	currentIdx := strings.Index(out, "  203")
	historicalIdx := strings.Index(out, "  249")
	if currentIdx == -1 || historicalIdx == -1 {
		t.Fatalf("expected both jobs in output, got:\n%s", out)
	}
	if strings.Contains(out, historicalLaunchJobsHeader) {
		t.Fatalf("expected attempts to remain inline, got:\n%s", out)
	}
	if !(currentIdx < historicalIdx) {
		t.Fatalf("expected previously current job to stay in run order, got:\n%s", out)
	}
	if !strings.Contains(out, "orphaned") {
		t.Fatalf("expected orphaned status in output, got:\n%s", out)
	}
}

func TestWatchModelView_OrphanedJobsUnderFailedInstanceShowInfraFailure(t *testing.T) {
	instanceID := int64(455)
	m := watchModel{
		instanceIDs: []int64{instanceID},
		updates: map[int64]campaign.InstanceUpdate{
			instanceID: {
				Launch: &db.Launch{
					ID:                instanceID,
					Status:            db.LaunchStatusFailed,
					Provider:          "vastai",
					GPUSpec:           "Tesla V100",
					TerminationReason: db.TerminationReasonInfraFailure,
				},
				Jobs: []*db.Job{
					{ID: 554, Status: db.StatusQueued, Description: "EXP-076"},
				},
				JobAttemptOutcomes: map[int64]string{
					554: db.AttemptOutcomeOrphaned,
				},
			},
		},
		jobProgressHWM: map[int64]int{},
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "terminated (infrastructure failure)") {
		t.Fatalf("expected infra failure instance reason, got:\n%s", out)
	}
	if !strings.Contains(out, "orphaned") {
		t.Fatalf("expected orphaned job outcome in view, got:\n%s", out)
	}
}

func TestWatchModelViewShowsCampaignSummaryRate(t *testing.T) {
	launchUnix := time.Now().Add(-2 * time.Hour).Unix()
	instanceA := int64(10)
	instanceB := int64(11)
	m := watchModel{
		instanceIDs: []int64{instanceA, instanceB},
		updates: map[int64]campaign.InstanceUpdate{
			instanceA: {
				Launch: &db.Launch{
					ID:               instanceA,
					Status:           db.LaunchStatusRunning,
					Provider:         "vastai",
					GPUSpec:          "A100",
					CostPerHourCents: 150,
					LaunchedAt:       &launchUnix,
				},
			},
			instanceB: {
				Launch: &db.Launch{
					ID:               instanceB,
					Status:           db.LaunchStatusFailed,
					Provider:         "vastai",
					GPUSpec:          "A100",
					CostPerHourCents: 200,
					LaunchedAt:       &launchUnix,
					EndedAt:          &launchUnix,
				},
			},
		},
		campaignID:     77,
		launchedAt:     time.Unix(launchUnix, 0),
		jobProgressHWM: map[int64]int{},
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "Summary: uptime:") {
		t.Fatalf("expected campaign uptime summary, got:\n%s", out)
	}
	if !strings.Contains(out, "current rate: $1.50/hr") {
		t.Fatalf("expected current rate to include only non-terminal instances, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlock_ShowsCPUAndGPULines(t *testing.T) {
	now := time.Now()
	update := campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:              487,
			Status:          db.LaunchStatusRunning,
			Provider:        "vastai",
			ResolvedGPUName: "RTX 4090",
			GPUMemGB:        24,
			CUDAVersion:     12.4,
		},
		Instance: &cloud.Instance{
			CPUName:  "AMD EPYC 7C13 64-Core Processor",
			CPUCores: 25,
			RAMGB:    1008,
		},
	}

	out := formatWatchInstanceBlock(update, map[int64]int{}, watchInstanceBlockOptions{plain: true, now: now})
	if !strings.Contains(out, "Instance wi487 — RTX 4090 24GB — running") {
		t.Fatalf("header missing GPU brief, got:\n%s", out)
	}
	if strings.Contains(out, "Specs:") {
		t.Fatalf("expected Specs: label removed, got:\n%s", out)
	}
	if !strings.Contains(out, "CPU: AMD EPYC 7C13 64-Core Processor, 25 cores, 1008 GB RAM") {
		t.Fatalf("expected CPU line, got:\n%s", out)
	}
	if !strings.Contains(out, "GPU: RTX 4090, 24GB per GPU, CUDA 12.4") {
		t.Fatalf("expected GPU detail line, got:\n%s", out)
	}
}

func stripANSI(s string) string {
	re := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	return re.ReplaceAllString(s, "")
}
