package terminal

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
)

func TestRenderJobListPlainTruncatesToWidth(t *testing.T) {
	jobs := []*db.Job{
		{
			ID:          42,
			Host:        "studio",
			Status:      db.StatusCompleted,
			StartTime:   1,
			WorkingDir:  "/workspace/project-alpha-with-a-long-name",
			Project:     "llm-performance-models",
			Description: "train model with a very long description that should not fit in a narrow terminal window",
			ExitCode:    testIntPtr(0),
		},
		{
			ID:          43,
			Host:        "",
			Status:      db.StatusQueued,
			Description: "queued hostless job",
		},
	}

	out := renderJobListPlain(jobs, 64)
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if got := lipgloss.Width(line); got > 64 {
			t.Fatalf("line width = %d, want <= 64: %q", got, line)
		}
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("expected truncated output, got:\n%s", out)
	}
	if !strings.Contains(out, "(unplaced)") {
		t.Fatalf("expected hostless job marker, got:\n%s", out)
	}
	if !strings.Contains(out, "PROJECT") {
		t.Fatalf("expected project header, got:\n%s", out)
	}
}

func TestRenderJobListPlainIncludesDirectoryOnWideTerminals(t *testing.T) {
	jobs := []*db.Job{
		{
			ID:          42,
			Host:        "cool30",
			Status:      db.StatusRunning,
			StartTime:   1,
			WorkingDir:  "/workspace/project-alpha",
			Project:     "llm-performance-models",
			Description: "train model",
		},
	}

	out := renderJobListPlain(jobs, 120)
	if !strings.Contains(out, "DIR") {
		t.Fatalf("output missing DIR header, got:\n%s", out)
	}
	if !strings.Contains(out, "PROJECT") {
		t.Fatalf("output missing PROJECT header, got:\n%s", out)
	}
	if !strings.Contains(out, "project-alpha") {
		t.Fatalf("output missing directory tail, got:\n%s", out)
	}
	if !strings.Contains(out, "llm-performance-models") {
		t.Fatalf("output missing project name, got:\n%s", out)
	}
}

func TestRenderJobListPlainProjectFallsBackToDirectoryTail(t *testing.T) {
	jobs := []*db.Job{
		{
			ID:          42,
			Host:        "cool30",
			Status:      db.StatusRunning,
			StartTime:   1,
			WorkingDir:  "/workspace/project-alpha",
			Description: "train model",
		},
	}

	out := renderJobListPlain(jobs, 80)
	if !strings.Contains(out, "PROJECT") {
		t.Fatalf("output missing PROJECT header, got:\n%s", out)
	}
	if !strings.Contains(out, "project-alpha") {
		t.Fatalf("expected directory-tail fallback in project column, got:\n%s", out)
	}
}

func TestListTUIEmptyStateText(t *testing.T) {
	m := listTUIModel{syncInProgress: true}
	if got := m.emptyStateText(); !strings.Contains(got, "Waiting for startup sync") {
		t.Fatalf("emptyStateText() = %q", got)
	}

	m.syncInProgress = false
	m.statusMessage = ""
	if got := m.emptyStateText(); got != "No jobs match this view." {
		t.Fatalf("emptyStateText() = %q", got)
	}
}

func TestListTUIJobsLoadedRefreshesRows(t *testing.T) {
	m := listTUIModel{
		jobs: []*db.Job{
			{ID: 1, Host: "studio", Status: db.StatusQueued, Description: "old"},
		},
		cursor: 0,
		offset: 0,
	}

	next, _ := m.Update(listJobsLoadedMsg{
		jobs: []*db.Job{
			{ID: 2, Host: "studio", Status: db.StatusRunning, Description: "new"},
			{ID: 1, Host: "studio", Status: db.StatusQueued, Description: "old"},
		},
	})

	got := next.(listTUIModel)
	if len(got.jobs) != 2 {
		t.Fatalf("job count = %d, want 2", len(got.jobs))
	}
	if got.jobs[0].ID != 2 {
		t.Fatalf("first job ID = %d, want 2", got.jobs[0].ID)
	}
}

func TestListTUIQuickLaunchProgressUpdatesStatus(t *testing.T) {
	progressCh := make(chan listQuickLaunchProgressMsg)
	m := listTUIModel{
		quickLaunchProgress: progressCh,
		statusMessage:       "Launching new instance...",
	}

	next, _ := m.Update(listQuickLaunchProgressMsg{message: "Planning placement..."})
	got := next.(listTUIModel)
	if got.statusMessage != "Planning placement..." {
		t.Fatalf("statusMessage = %q, want %q", got.statusMessage, "Planning placement...")
	}
}

func TestListTUIQuickLaunchDonePinsStatusAgainstAutoPilotNoise(t *testing.T) {
	m := listTUIModel{
		autoMode: true,
	}
	next, _ := m.Update(listQuickLaunchDoneMsg{
		instanceIDs:  []int64{675},
		runningJobID: 702,
	})
	got := next.(listTUIModel)
	if !strings.Contains(got.statusMessage, "job #702 running") {
		t.Fatalf("statusMessage = %q, want running job summary", got.statusMessage)
	}

	next2, _ := got.Update(listAutoPilotDoneMsg{})
	got2 := next2.(listTUIModel)
	if got2.statusMessage != got.statusMessage {
		t.Fatalf("statusMessage overwritten during hold: got %q, want %q", got2.statusMessage, got.statusMessage)
	}
	if time.Now().After(got2.quickLaunchStatusHoldUntil) {
		t.Fatalf("quick launch status hold should be set in the future")
	}
}

func TestListTUIQuickLaunchInFlightProtectsStatusFromAutoPilotNoise(t *testing.T) {
	m := listTUIModel{
		autoMode:       true,
		quickLaunching: true,
		statusMessage:  "Launching new instance...",
	}
	next, _ := m.Update(listAutoPilotDoneMsg{})
	got := next.(listTUIModel)
	if got.statusMessage != "Launching new instance..." {
		t.Fatalf("statusMessage overwritten while quick launch in-flight: got %q", got.statusMessage)
	}
}

func TestListTUIGroupedViewShowsStatusAndControlsOnSeparateLines(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		autoMode:        true,
		width:           90,
		height:          12,
		title:           "Jobs",
		jobs: []*db.Job{
			{ID: 733, Status: db.StatusQueued, Description: "retry pending", Project: "proj"},
		},
		statusMessage: "Auto-pilot failed: submit jobs to instance control plane: get object grace/686/acks/17755648...",
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "[1 jobs] Auto-pilot failed:") {
		t.Fatalf("expected status line in grouped footer, got:\n%s", out)
	}
	if !strings.Contains(out, "a:toggle-auto auto:ON q:quit") {
		t.Fatalf("expected controls line with auto state, got:\n%s", out)
	}
}

func TestListTUIGroupedViewKeepsControlsVisibleWhenStatusIsLong(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		autoMode:        true,
		width:           64,
		height:          12,
		title:           "Jobs",
		jobs: []*db.Job{
			{ID: 733, Status: db.StatusQueued, Description: "retry pending", Project: "proj"},
		},
		statusMessage: "Auto-pilot failed: no cloud providers available: vastai: vastai CLI availability check failed: timeout",
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "a:toggle-auto auto:ON q:quit") {
		t.Fatalf("expected controls line to remain visible even with long status, got:\n%s", out)
	}
}

func TestListTUIGroupedViewPlacesSharedStatusAboveControls(t *testing.T) {
	database := db.SetupTestDB(t)
	m := listTUIModel{
		database:        database,
		groupedByStatus: true,
		autoMode:        true,
		width:           90,
		height:          12,
		title:           "Jobs",
		jobs: []*db.Job{
			{ID: 733, Status: db.StatusQueued, Description: "retry pending", Project: "proj"},
		},
		statusMessage: "Auto-pilot: monitoring",
	}

	out := stripANSI(m.View())
	groupedIdx := strings.Index(out, "[1 jobs] Auto-pilot: monitoring")
	sharedIdx := strings.Index(out, "status: running jobs")
	controlsIdx := strings.Index(out, "a:toggle-auto auto:ON q:quit")
	if groupedIdx < 0 || sharedIdx < 0 || controlsIdx < 0 {
		t.Fatalf("missing grouped footer parts, got:\n%s", out)
	}
	if !(groupedIdx < sharedIdx && sharedIdx < controlsIdx) {
		t.Fatalf("expected grouped status -> shared status -> controls order, got:\n%s", out)
	}
	if etaIdx := strings.Index(out, "ETA: "); etaIdx >= 0 && !(groupedIdx < etaIdx && etaIdx < sharedIdx) {
		t.Fatalf("expected ETA line between grouped status and shared status, got:\n%s", out)
	}
}

func TestListTUIAutoPilotFailureDoesNotStopSubsequentTicks(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		autoMode:        true,
		autoInProgress:  true,
		database:        &sql.DB{},
	}

	next, _ := m.Update(listAutoPilotDoneMsg{err: errors.New("boom")})
	got := next.(listTUIModel)
	if got.autoInProgress {
		t.Fatal("autoInProgress should be cleared after autopilot failure")
	}
	if !strings.Contains(got.statusMessage, "Auto-pilot failed: boom") {
		t.Fatalf("statusMessage = %q, want failure text", got.statusMessage)
	}

	next2, _ := got.Update(listSyncTickMsg{})
	got2 := next2.(listTUIModel)
	if !got2.autoInProgress {
		t.Fatal("expected next sync tick to schedule another autopilot pass")
	}
}

func TestListTUIAutoPilotFailureSummarizesGraceAckError(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		autoMode:        true,
		autoInProgress:  true,
		database:        &sql.DB{},
	}

	err := errors.New("submit jobs to instance control plane: get object grace/728/acks/1775574400000-728-123456.json: operation error S3: GetObject, https response error StatusCode: 404")
	next, _ := m.Update(listAutoPilotDoneMsg{err: err})
	got := next.(listTUIModel)

	if !strings.Contains(got.statusMessage, "instance wi728 did not acknowledge queued jobs") {
		t.Fatalf("statusMessage = %q, want instance-specific summary", got.statusMessage)
	}
	if !strings.Contains(got.statusMessage, "run `weft sync`") {
		t.Fatalf("statusMessage = %q, want actionable guidance", got.statusMessage)
	}
}

func TestListTUIAutoPilotStatusUsesSingularInstanceWordingAndClass(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		autoMode:        true,
	}

	next, _ := m.Update(listAutoPilotDoneMsg{
		launched:      1,
		launchedClass: "A100 80GB",
	})
	got := next.(listTUIModel)
	if got.statusMessage != "Auto-pilot: launched 1 A100 80GB instance" {
		t.Fatalf("statusMessage = %q", got.statusMessage)
	}
}

func TestListTUIAutoPilotStatusUsesPluralInstancesWording(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		autoMode:        true,
	}

	next, _ := m.Update(listAutoPilotDoneMsg{launched: 2})
	got := next.(listTUIModel)
	if got.statusMessage != "Auto-pilot: launched 2 instances" {
		t.Fatalf("statusMessage = %q", got.statusMessage)
	}
}
