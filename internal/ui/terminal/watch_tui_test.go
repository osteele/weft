package terminal

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/degraded"
	"github.com/osteele/weft/internal/instanceintent"
)

func TestSystemWatchModelViewShowsSectionsAndDirectoryTails(t *testing.T) {
	cloudInstance := &db.Launch{
		ID:       5,
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	}

	m := watchModel{
		mode:   watchModeSystem,
		width:  120,
		height: 20,
		cloudInstances: []*db.Launch{
			cloudInstance,
		},
		instanceIDs: []int64{5},
		updates: map[int64]campaign.InstanceUpdate{
			5: {
				Launch: cloudInstance,
				Jobs: []*db.Job{
					{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/project-alpha", Project: "EXP-ALPHA", Description: "train model"},
					{ID: 89, Status: db.StatusQueued, WorkingDir: "/workspace/project-delta", Project: "EXP-DELTA", Description: "eval model"},
				},
			},
		},
		onPremHosts: []onPremHostSummary{
			{
				Name: "cool30",
				Jobs: []*db.Job{
					{ID: 41, Status: db.StatusRunning, Host: "cool30", WorkingDir: "/tmp/project-beta", Project: "BETA", Description: "eval model"},
				},
			},
		},
		unplacedJobs: []*db.Job{
			{ID: 123, Status: db.StatusQueued, WorkingDir: "/tmp/project-gamma", Project: "GAMMA", Description: "benchmark", GPUClass: "A100"},
		},
		jobProgressHWM: map[int64]int{},
		cloudReason:    degraded.CloudLastKnownRentalsReason(),
	}

	out := stripANSI(m.View())
	for _, expected := range []string{
		"Rental Instances (1)",
		"note: " + degraded.CloudLastKnownRentalsReason(),
		"Inventory Hosts (1 active)",
		"Unplaced Jobs (1)",
		"[u] unplace",
		"Instance 5 — A100 — running",
		"  vastai:",
		"  Jobs: 0/2 resolved",
		"EXP-ALPHA",
		"EXP-DELTA",
		"BETA",
		"GAMMA",
		"cool30",
	} {
		if !strings.Contains(out, expected) {
			t.Fatalf("output missing %q, got:\n%s", expected, out)
		}
	}
	if strings.Contains(out, "ID 5") {
		t.Fatalf("output should omit redundant provider line ID, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockShowsCampaignStyleLayout(t *testing.T) {
	ci := &db.Launch{
		ID:                 5,
		Status:             db.LaunchStatusRunning,
		Provider:           "vastai",
		ProviderInstanceID: "32734388",
		GPUSpec:            "A100",
	}
	update := campaign.InstanceUpdate{
		Launch: ci,
		Jobs: []*db.Job{
			{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/project-alpha", Project: "EXP-ALPHA", Description: "train model"},
			{ID: 89, Status: db.StatusQueued, WorkingDir: "/workspace/project-delta", Project: "EXP-DELTA", Description: "eval model"},
		},
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{}))
	for _, expected := range []string{
		"Instance 5 — A100 — running",
		"  vastai: 32734388",
		"  Jobs: 0/2 resolved",
		"EXP-ALPHA",
		"EXP-DELTA",
	} {
		if !strings.Contains(out, expected) {
			t.Fatalf("output missing %q, got:\n%s", expected, out)
		}
	}
	if strings.Contains(out, "ID 5") {
		t.Fatalf("output should omit redundant provider line ID, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockUsesDBStatusWhenProviderLoading(t *testing.T) {
	ci := &db.Launch{
		ID:                 111,
		Status:             db.LaunchStatusRunning,
		Provider:           "vastai",
		ProviderInstanceID: "32740493",
		GPUSpec:            "A100",
	}
	update := campaign.InstanceUpdate{
		Launch:   ci,
		Instance: &cloud.Instance{Status: cloud.ProviderStatusLoading},
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{}))
	if !strings.Contains(out, "Instance 111 — A100 — bootstrapping") {
		t.Fatalf("expected conservative bootstrap status in header, got:\n%s", out)
	}
	if !strings.Contains(out, "Bootstrap: waiting for bootstrap activity") {
		t.Fatalf("expected bootstrap fallback in output, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockShowsObservedDBRunningPhase(t *testing.T) {
	instanceID := int64(112)
	update := campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:                 instanceID,
			Status:             db.LaunchStatusRunning,
			Provider:           "vastai",
			ProviderInstanceID: "32740494",
			GPUSpec:            "A100",
		},
		Jobs: []*db.Job{
			{ID: 203, Status: db.StatusRunning, LaunchID: &instanceID, Description: "current"},
			{ID: 249, Status: db.StatusQueued, Description: "queued"},
		},
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{}))
	if !strings.Contains(out, "Phase: running job 203 (observed from DB)") {
		t.Fatalf("expected DB-observed phase fallback, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockShowsProvisioningFallbackWithoutProviderID(t *testing.T) {
	update := campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:       113,
			Status:   db.LaunchStatusLaunching,
			Provider: "vastai",
			GPUSpec:  "A100",
		},
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{}))
	if !strings.Contains(out, "  vastai: (provisioning...)") {
		t.Fatalf("expected provisioning provider line, got:\n%s", out)
	}
	if !strings.Contains(out, "Bootstrap: provisioning instance") {
		t.Fatalf("expected provisioning bootstrap fallback, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockShowsObservabilityDetails(t *testing.T) {
	changedAt := time.Now().Add(-4 * time.Second)
	ci := &db.Launch{
		ID:                 106,
		Status:             db.LaunchStatusRunning,
		Provider:           "vastai",
		ProviderInstanceID: "32712486",
		GPUSpec:            "A100",
	}
	update := campaign.InstanceUpdate{
		Launch:         ci,
		InstancePhase:  "uploading-results:88",
		PhaseChangedAt: &changedAt,
		TerminationIntent: &instanceintent.Marker{
			TerminalStatus:  db.LaunchStatusFailed,
			RequestedAtUnix: time.Now().Add(-2 * time.Second).Unix(),
			DestroyAttempts: 2,
			LastError:       "exit status 22",
		},
		Jobs: []*db.Job{
			{ID: 88, Status: db.StatusFailed, WorkingDir: "/workspace/project-alpha", Description: "train model"},
		},
		JobPhaseTimings: map[int64]*db.JobPhaseTimings{
			88: {
				UploadWorkspaceBytes:  watchTestInt64Ptr(4096),
				OutputUploadFiles:     watchTestIntPtr(2),
				OutputUploadDuration:  watchTestInt64Ptr(250),
				UploadResultsBytes:    watchTestInt64Ptr(8192),
				ResultsUploadFiles:    watchTestIntPtr(4),
				ResultsUploadDuration: watchTestInt64Ptr(600),
			},
		},
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{now: time.Now()}))
	for _, want := range []string{
		"Phase: uploading logs/results (job 88) (for",
		"Cleanup:",
		"Status: destroy request failed; retry pending",
		"uploads: outputs 2 files, 4.0 KiB, 250ms | logs 4 files, 8.0 KiB, 600ms",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestFormatWatchInstanceBlockShowsActualSpendWithoutProviderInstance(t *testing.T) {
	update := campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:               106,
			Status:           db.LaunchStatusRunning,
			Provider:         "vastai",
			ActualSpendCents: 1234,
			GPUSpec:          "A100",
		},
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{now: time.Now()}))
	if !strings.Contains(out, "Cost: $12.34") {
		t.Fatalf("expected actual spend fallback in output, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockShowsDBHourlyCostFallback(t *testing.T) {
	now := time.Unix(7200, 0)
	launchedAt := int64(0)
	update := campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:               107,
			Status:           db.LaunchStatusRunning,
			Provider:         "vastai",
			CostPerHourCents: 150,
			LaunchedAt:       &launchedAt,
			GPUSpec:          "A100",
		},
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{now: now}))
	if !strings.Contains(out, "Cost: $3.00 (uptime: 2h0m0s, rate: $1.50/hr)") {
		t.Fatalf("expected DB hourly cost fallback in output, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockClampsPhaseDurationToInstanceUptime(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	launchedAt := now.Add(-2*time.Minute - 24*time.Second)
	update := campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:                 142,
			Status:             db.LaunchStatusRunning,
			Provider:           "vastai",
			ProviderInstanceID: "32896734",
			GPUSpec:            "A100 >=40GB",
			ResolvedGPUName:    "A100 SXM4",
			CostPerHourCents:   66,
			LaunchedAt:         watchTestInt64Ptr(launchedAt.Unix()),
		},
		InstancePhase:  "setup:175",
		PhaseChangedAt: watchTimePtr(launchedAt),
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{now: now}))
	if !strings.Contains(out, "Phase: setup (job 175) (for 2m24s)") {
		t.Fatalf("expected clamped phase duration, got:\n%s", out)
	}
	if !strings.Contains(out, "Cost: $0.03 (uptime: 2m24s, rate: $0.66/hr)") {
		t.Fatalf("expected matching uptime, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockUsesEndedAtForTerminalUptime(t *testing.T) {
	now := time.Unix(7200, 0)
	launchedAt := int64(0)
	endedAt := int64(3600)
	update := campaign.InstanceUpdate{
		Launch: &db.Launch{
			ID:               108,
			Status:           db.LaunchStatusFailed,
			Provider:         "vastai",
			CostPerHourCents: 150,
			LaunchedAt:       &launchedAt,
			EndedAt:          &endedAt,
			GPUSpec:          "A100",
		},
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{now: now}))
	if !strings.Contains(out, "Cost: $1.50 (uptime: 1h0m0s, rate: $1.50/hr)") {
		t.Fatalf("expected terminal uptime to stop at ended_at, got:\n%s", out)
	}
}

func watchTimePtr(t time.Time) *time.Time {
	return &t
}

func TestUpdateWatchJobProgressHWMPrunesDisappearedJobs(t *testing.T) {
	hwm := map[int64]int{
		88: 40,
		89: 20,
	}
	prev := campaign.InstanceUpdate{
		Jobs: []*db.Job{
			{ID: 88, Status: db.StatusRunning},
			{ID: 89, Status: db.StatusRunning},
		},
	}
	curr := campaign.InstanceUpdate{
		Jobs: []*db.Job{
			{ID: 89, Status: db.StatusRunning},
		},
	}

	updateWatchJobProgressHWM(hwm, prev, curr)

	if _, ok := hwm[88]; ok {
		t.Fatalf("expected disappeared job progress entry to be pruned, got %+v", hwm)
	}
	if got := hwm[89]; got != 20 {
		t.Fatalf("job 89 progress = %d, want 20", got)
	}
}

func TestSystemWatchModelViewShowsCloudSummaryRate(t *testing.T) {
	launchedAt := time.Now().Add(-2 * time.Hour).Unix()
	instanceID := int64(51)
	m := watchModel{
		mode:   watchModeSystem,
		width:  120,
		height: 16,
		cloudInstances: []*db.Launch{
			{
				ID:               instanceID,
				Status:           db.LaunchStatusRunning,
				Provider:         "vastai",
				GPUSpec:          "A100",
				CostPerHourCents: 150,
				LaunchedAt:       &launchedAt,
			},
		},
		instanceIDs: []int64{instanceID},
		updates: map[int64]campaign.InstanceUpdate{
			instanceID: {
				Launch: &db.Launch{
					ID:               instanceID,
					Status:           db.LaunchStatusRunning,
					Provider:         "vastai",
					GPUSpec:          "A100",
					CostPerHourCents: 150,
					LaunchedAt:       &launchedAt,
				},
			},
		},
		jobProgressHWM: map[int64]int{},
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "Summary:  cost:") {
		t.Fatalf("expected cloud summary line, got:\n%s", out)
	}
	if !strings.Contains(out, "current rate: $1.50/hr") {
		t.Fatalf("expected cloud summary rate, got:\n%s", out)
	}
}

func TestFormatOnPremJobRowQueuedUsesDashDuration(t *testing.T) {
	m := watchModel{mode: watchModeSystem}
	row := stripANSI(m.formatOnPremJobRow(&db.Job{
		ID:          41,
		Status:      db.StatusQueued,
		Host:        "cool30",
		WorkingDir:  "/tmp/project-beta",
		Description: "eval",
	}, 12))

	if !strings.Contains(row, "queued") {
		t.Fatalf("expected status 'queued' in row %q", row)
	}
	if !strings.Contains(row, "—") {
		t.Fatalf("expected dash duration in row %q", row)
	}
	if !strings.HasSuffix(strings.TrimSpace(row), "eval") {
		t.Fatalf("expected description last in row %q", row)
	}
}

func TestSystemWatchModelUnplaceDoneMovesJobImmediately(t *testing.T) {
	m := watchModel{
		mode: watchModeSystem,
		onPremHosts: []onPremHostSummary{
			{
				Name: "cool30",
				Jobs: []*db.Job{
					{ID: 41, Status: db.StatusQueued, Host: "cool30", WorkingDir: "/tmp/project-beta", Description: "eval model"},
				},
			},
		},
		updates:        map[int64]campaign.InstanceUpdate{},
		jobProgressHWM: map[int64]int{},
	}

	msg := watchUnplaceDoneMsg{
		job: &db.Job{
			ID:          41,
			Status:      db.StatusQueued,
			Host:        "",
			WorkingDir:  "/tmp/project-beta",
			Description: "eval model",
			Tags:        []string{db.TagCloud},
		},
		message: "moved",
	}

	updatedModel, _ := m.Update(msg)
	got := updatedModel.(watchModel)

	if len(got.onPremHosts) != 0 {
		t.Fatalf("expected on-prem host list to be empty, got %+v", got.onPremHosts)
	}
	if len(got.unplacedJobs) != 1 {
		t.Fatalf("expected one unplaced job, got %+v", got.unplacedJobs)
	}
	if got.unplacedJobs[0].ID != 41 {
		t.Fatalf("unplaced job ID = %d, want 41", got.unplacedJobs[0].ID)
	}
	if got.unplacedJobs[0].Host != "" {
		t.Fatalf("unplaced job host = %q, want empty", got.unplacedJobs[0].Host)
	}
}

func TestSystemWatchModelViewShowsSelectedUnplacedReasonInFooter(t *testing.T) {
	m := watchModel{
		mode:   watchModeSystem,
		width:  180,
		height: 12,
		cursor: 0,
		unplacedJobs: []*db.Job{
			{
				ID:               189,
				Status:           db.StatusQueued,
				WorkingDir:       "/tmp/project-gamma",
				Description:      "benchmark",
				GPUClass:         "L40s",
				PlacementReasons: []string{"no local host matched gpu-class=L40s, gpu-mem>=20GB", "2 hosts: no L40s GPU"},
			},
		},
		updates:        map[int64]campaign.InstanceUpdate{},
		jobProgressHWM: map[int64]int{},
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"#189 unplaced: no local host matched gpu-class=L40s, gpu-mem>=20GB | 2 hosts: no L40s GPU",
		"[u] unplace",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestSystemWatchModelViewTruncatesSelectedUnplacedReasonInFooter(t *testing.T) {
	m := watchModel{
		mode:   watchModeSystem,
		width:  120,
		height: 12,
		cursor: 0,
		unplacedJobs: []*db.Job{
			{
				ID:               189,
				Status:           db.StatusQueued,
				WorkingDir:       "/tmp/project-gamma",
				Description:      "benchmark",
				GPUClass:         "L40s",
				PlacementReasons: []string{"no local host matched gpu-class=L40s, gpu-mem>=20GB", "2 hosts: no L40s GPU", "another long reason"},
			},
		},
		updates:        map[int64]campaign.InstanceUpdate{},
		jobProgressHWM: map[int64]int{},
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "#189 unplaced:") {
		t.Fatalf("footer missing unplaced prefix, got:\n%s", out)
	}
	if !strings.Contains(out, "...") {
		t.Fatalf("footer should truncate detail, got:\n%s", out)
	}
	if strings.Contains(out, "another long reason") {
		t.Fatalf("footer should omit overflowing detail, got:\n%s", out)
	}
}

func TestSystemWatchModelSelectedCloudJob(t *testing.T) {
	cloudInstance := &db.Launch{
		ID:       5,
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	}
	job88 := &db.Job{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/proj", Description: "train"}
	job89 := &db.Job{ID: 89, Status: db.StatusQueued, WorkingDir: "/workspace/proj", Description: "eval"}

	m := watchModel{
		mode:           watchModeSystem,
		width:          120,
		height:         20,
		cloudInstances: []*db.Launch{cloudInstance},
		instanceIDs:    []int64{5},
		updates: map[int64]campaign.InstanceUpdate{
			5: {
				Launch: cloudInstance,
				Jobs:   []*db.Job{job88, job89},
			},
		},
		jobProgressHWM: map[int64]int{},
	}

	// cursor 0 = instance header → no cloud job selected
	m.cursor = 0
	if got := m.selectedCloudJob(); got != nil {
		t.Errorf("cursor 0: expected nil (instance header), got job %d", got.ID)
	}

	// cursor 1 = first job row (job 88)
	m.cursor = 1
	if got := m.selectedCloudJob(); got == nil || got.ID != 88 {
		t.Errorf("cursor 1: expected job 88, got %v", got)
	}

	// cursor 2 = second job row (job 89)
	m.cursor = 2
	if got := m.selectedCloudJob(); got == nil || got.ID != 89 {
		t.Errorf("cursor 2: expected job 89, got %v", got)
	}

	// cursor 3 = past cloud section → nil
	m.cursor = 3
	if got := m.selectedCloudJob(); got != nil {
		t.Errorf("cursor 3: expected nil, got job %d", got.ID)
	}
}

func TestSystemWatchModelSelectableRowCountIncludesCloudJobs(t *testing.T) {
	cloudInstance := &db.Launch{
		ID:       5,
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	}

	m := watchModel{
		mode:           watchModeSystem,
		width:          120,
		height:         20,
		cloudInstances: []*db.Launch{cloudInstance},
		instanceIDs:    []int64{5},
		updates: map[int64]campaign.InstanceUpdate{
			5: {
				Launch: cloudInstance,
				Jobs: []*db.Job{
					{ID: 88, Status: db.StatusRunning},
					{ID: 89, Status: db.StatusQueued},
				},
			},
		},
		onPremHosts: []onPremHostSummary{
			{Name: "cool30", Jobs: []*db.Job{{ID: 41, Status: db.StatusRunning, Host: "cool30"}}},
		},
		unplacedJobs:   []*db.Job{{ID: 123, Status: db.StatusQueued}},
		jobProgressHWM: map[int64]int{},
	}

	// 1 instance header + 2 job rows + 1 on-prem job + 1 unplaced = 5
	if got := m.selectableRowCount(); got != 5 {
		t.Errorf("selectableRowCount() = %d, want 5", got)
	}
}

func TestInstanceWatchModelViewShowsInventoryHosts(t *testing.T) {
	cloudInstance := &db.Launch{
		ID:       5,
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	}

	m := watchModel{
		mode:        watchModeInstances,
		width:       120,
		height:      20,
		instanceIDs: []int64{5},
		updates: map[int64]campaign.InstanceUpdate{
			5: {
				Launch: cloudInstance,
				Jobs: []*db.Job{
					{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/proj", Project: "EXP-ALPHA", Description: "train"},
				},
			},
		},
		onPremHosts: []onPremHostSummary{
			{
				Name: "cool30",
				Jobs: []*db.Job{
					{ID: 41, Status: db.StatusQueued, Host: "cool30", WorkingDir: "/tmp/project-beta", Project: "BETA", Description: "eval model"},
				},
			},
		},
		jobProgressHWM: map[int64]int{},
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"Instance 5 — A100 — running",
		"Inventory Hosts (1 active)",
		"cool30",
		"BETA",
		"eval model",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestInstanceWatchModelSelectableRowCountIncludesOnPremJobs(t *testing.T) {
	cloudInstance := &db.Launch{
		ID:       5,
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	}

	m := watchModel{
		mode:        watchModeInstances,
		width:       120,
		height:      20,
		instanceIDs: []int64{5},
		updates: map[int64]campaign.InstanceUpdate{
			5: {
				Launch: cloudInstance,
				Jobs: []*db.Job{
					{ID: 88, Status: db.StatusRunning},
					{ID: 89, Status: db.StatusQueued},
				},
			},
		},
		onPremHosts: []onPremHostSummary{
			{Name: "cool30", Jobs: []*db.Job{{ID: 41, Status: db.StatusRunning, Host: "cool30"}}},
		},
		unplacedJobs:   []*db.Job{{ID: 123, Status: db.StatusQueued}},
		jobProgressHWM: map[int64]int{},
	}

	// 1 instance header + 2 cloud jobs + 1 on-prem job + 1 unplaced
	if got := m.selectableRowCount(); got != 5 {
		t.Errorf("selectableRowCount() = %d, want 5", got)
	}
}

func TestInstanceWatchModelSelectedOnPremJob(t *testing.T) {
	cloudInstance := &db.Launch{
		ID:       5,
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	}
	job88 := &db.Job{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/proj", Description: "train"}
	onPremJob := &db.Job{ID: 41, Status: db.StatusQueued, Host: "cool30", WorkingDir: "/tmp/project-beta", Description: "eval"}

	m := watchModel{
		mode:        watchModeInstances,
		width:       120,
		height:      20,
		instanceIDs: []int64{5},
		updates: map[int64]campaign.InstanceUpdate{
			5: {
				Launch: cloudInstance,
				Jobs:   []*db.Job{job88},
			},
		},
		onPremHosts:    []onPremHostSummary{{Name: "cool30", Jobs: []*db.Job{onPremJob}}},
		jobProgressHWM: map[int64]int{},
	}

	m.cursor = 2 // cloud header + one cloud job, then first on-prem job
	if got := m.selectedOnPremJob(); got == nil || got.ID != onPremJob.ID {
		t.Fatalf("cursor 2: expected on-prem job %d, got %v", onPremJob.ID, got)
	}
}

func TestInstanceWatchModelUpdate_OnPremRefreshUpdatesHosts(t *testing.T) {
	m := watchModel{
		mode:           watchModeInstances,
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		jobProgressHWM: map[int64]int{},
	}

	updatedModel, _ := m.Update(watchOnPremRefreshedMsg{
		updateOnPremHosts: true,
		onPremHosts: []onPremHostSummary{
			{Name: "cool30", Jobs: []*db.Job{{ID: 41, Status: db.StatusQueued, Host: "cool30"}}},
		},
		updateUnplacedJobs: true,
		unplacedJobs:       []*db.Job{{ID: 123, Status: db.StatusQueued}},
	})
	got := updatedModel.(watchModel)

	if len(got.onPremHosts) != 1 || got.onPremHosts[0].Name != "cool30" {
		t.Fatalf("onPremHosts = %+v, want cool30", got.onPremHosts)
	}
	if len(got.unplacedJobs) != 1 || got.unplacedJobs[0].ID != 123 {
		t.Fatalf("unplacedJobs = %+v, want job 123", got.unplacedJobs)
	}
}

func TestInstanceWatchModelUpdate_UnplacedOnlyRefreshPreservesOnPremHosts(t *testing.T) {
	m := watchModel{
		mode: watchModeInstances,
		onPremHosts: []onPremHostSummary{
			{Name: "cool30", Jobs: []*db.Job{{ID: 41, Status: db.StatusQueued, Host: "cool30"}}},
		},
		unplacedJobs: []*db.Job{{ID: 88, Status: db.StatusQueued}},
		updates:      map[int64]campaign.InstanceUpdate{},
		channels:     map[int64]<-chan campaign.InstanceUpdate{},
		clients:      map[int64]cloud.Client{},
	}

	updatedModel, _ := m.Update(watchOnPremRefreshedMsg{
		updateUnplacedJobs: true,
		unplacedJobs:       []*db.Job{{ID: 123, Status: db.StatusQueued}},
	})
	got := updatedModel.(watchModel)

	if len(got.onPremHosts) != 1 || got.onPremHosts[0].Name != "cool30" {
		t.Fatalf("onPremHosts = %+v, want preserved cool30", got.onPremHosts)
	}
	if len(got.unplacedJobs) != 1 || got.unplacedJobs[0].ID != 123 {
		t.Fatalf("unplacedJobs = %+v, want job 123", got.unplacedJobs)
	}
}

func TestWatchModelHandleKey_BudgetRetryDoublesInstanceMultiplier(t *testing.T) {
	failedID := int64(88)
	m := watchModel{
		mode:        watchModeInstances,
		instanceIDs: []int64{failedID},
		cursor:      0, // instance header row
		updates: map[int64]campaign.InstanceUpdate{
			failedID: {
				Launch: &db.Launch{
					ID:                failedID,
					Status:            db.LaunchStatusFailed,
					TerminationReason: db.TerminationReasonProviderFailure,
				},
			},
		},
		retryBudgetMultiplier: map[int64]float64{},
		budgetBlockedFailed:   map[int64]bool{failedID: true},
	}

	updated, cmd := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'B'}})
	got := updated.(watchModel)

	if !got.retrying {
		t.Fatal("expected retrying=true after B")
	}
	if got.retryBudgetMultiplier[failedID] != 2 {
		t.Fatalf("retryBudgetMultiplier[%d] = %.1f, want 2.0", failedID, got.retryBudgetMultiplier[failedID])
	}
	if cmd == nil {
		t.Fatal("expected retry command after B")
	}
}

func TestWatchModelHandleKey_TogglesHelpOverlayInInstanceMode(t *testing.T) {
	m := watchModel{
		mode:                  watchModeInstances,
		width:                 120,
		height:                20,
		retryBudgetMultiplier: map[int64]float64{},
	}
	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	got := updated.(watchModel)
	if !got.projectHelp {
		t.Fatal("expected help overlay to open")
	}
	view := stripANSI(got.View())
	if !strings.Contains(view, "Watch Keybindings") {
		t.Fatalf("expected watch help view, got:\n%s", view)
	}

	updated, _ = got.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	got = updated.(watchModel)
	if got.projectHelp {
		t.Fatal("expected help overlay to close on Esc")
	}
}

func watchTestInt64Ptr(v int64) *int64 { return &v }

func watchTestIntPtr(v int) *int { return &v }
