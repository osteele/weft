package terminal

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
	if !strings.Contains(out, "Auto-pilot failed:") {
		t.Fatalf("expected status line in grouped footer, got:\n%s", out)
	}
	if !strings.Contains(out, "a:auto (ON)") {
		t.Fatalf("expected controls line with auto state, got:\n%s", out)
	}
	if !strings.Contains(out, "q:quit") {
		t.Fatalf("expected quit hint in controls line, got:\n%s", out)
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
	if !strings.Contains(out, "a:auto (ON)") || !strings.Contains(out, "q:quit") {
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
	sharedIdx := strings.Index(out, "0 jobs running")
	statusIdx := strings.Index(out, "Auto-pilot: monitoring")
	controlsIdx := strings.Index(out, "a:auto (ON)")
	if sharedIdx < 0 || statusIdx < 0 || controlsIdx < 0 {
		t.Fatalf("missing grouped footer parts, got:\n%s", out)
	}
	if !(sharedIdx < statusIdx && statusIdx < controlsIdx) {
		t.Fatalf("expected shared status -> status -> controls order, got:\n%s", out)
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

func TestListTUIAutoPilotFailureRebuildsGroupedRowsWithBlockReasons(t *testing.T) {
	jobs := []*db.Job{
		{ID: 10, Description: "test job", Tags: []string{"rental"}, Status: db.StatusQueued},
	}
	m := listTUIModel{
		groupedByStatus: true,
		autoMode:        true,
		autoInProgress:  true,
		database:        &sql.DB{},
		jobs:            jobs,
		width:           120,
		height:          40,
	}
	m.rebuildGroupedRows()

	// Simulate auto-pilot error with partial block reasons.
	reasons := map[int64]string{10: "no cloud providers available"}
	next, _ := m.Update(listAutoPilotDoneMsg{
		err:            errors.New("no cloud providers available"),
		blockedReasons: reasons,
	})
	got := next.(listTUIModel)

	if got.autoBlockReasons == nil {
		t.Fatal("autoBlockReasons should be set even on error")
	}
	if got.autoBlockReasons[10] != "no cloud providers available" {
		t.Fatalf("autoBlockReasons[10] = %q, want block reason", got.autoBlockReasons[10])
	}
	// Grouped rows should have been rebuilt to include the block reason.
	found := false
	for _, row := range got.groupedRows {
		if strings.Contains(row.text, "blocked:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected grouped rows to contain a blocked reason row after auto-pilot error")
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

func TestListTUIGroupedSelectionSkipsHeaders(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		width:           100,
		height:          20,
		jobs: []*db.Job{
			{ID: 101, Status: db.StatusRunning, Description: "running"},
			{ID: 102, Status: db.StatusQueued, Description: "queued"},
		},
	}
	m.rebuildGroupedRows()

	if len(m.groupedSelectableRows) != 2 {
		t.Fatalf("selectable rows = %d, want 2", len(m.groupedSelectableRows))
	}
	if row := m.selectedGroupedRow(); row < 0 || row >= len(m.groupedRows) || m.groupedRows[row].isHeader {
		t.Fatalf("initial selected row should be a job row, got row=%d", row)
	}
	if job := m.selectedGroupedJob(); job == nil || job.ID != 101 {
		t.Fatalf("initial selected job = %+v, want ID 101", job)
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	got := next.(listTUIModel)
	if job := got.selectedGroupedJob(); job == nil || job.ID != 102 {
		t.Fatalf("after down selected job = %+v, want ID 102", job)
	}
}

func TestListTUIGroupedControlsShowMoveForQueuedSelection(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		autoMode:        true,
		width:           100,
		height:          20,
		jobs: []*db.Job{
			{ID: 202, Status: db.StatusQueued, Description: "queued"},
		},
	}
	m.rebuildGroupedRows()

	line := m.groupedControlsText(true)
	if !strings.Contains(line, "k:kill") || !strings.Contains(line, "u:unplace") || !strings.Contains(line, "m:move") {
		t.Fatalf("controls line missing queued-job actions: %q", line)
	}
}

func TestListTUIGroupedEscCancelsPendingMoveLookup(t *testing.T) {
	m := listTUIModel{
		groupedByStatus:     true,
		width:               100,
		height:              20,
		moveLookupPending:   true,
		moveLookupRequestID: 7,
		statusMessage:       "Searching move destinations...",
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := next.(listTUIModel)
	if got.moveLookupPending {
		t.Fatalf("moveLookupPending = true, want false")
	}
	if got.moveLookupRequestID != 0 {
		t.Fatalf("moveLookupRequestID = %d, want 0", got.moveLookupRequestID)
	}
	if got.statusMessage != "Move lookup canceled" {
		t.Fatalf("statusMessage = %q, want %q", got.statusMessage, "Move lookup canceled")
	}
}

func TestListTUIGroupedIgnoresStaleMoveOptionsResult(t *testing.T) {
	m := listTUIModel{
		groupedByStatus:     true,
		width:               100,
		height:              20,
		moveLookupPending:   true,
		moveLookupRequestID: 9,
		statusMessage:       "Searching...",
	}

	next, _ := m.Update(listMoveOptionsReadyMsg{
		requestID: 8,
		jobID:     12,
		options:   []moveOption{{isNew: true, gpuName: "A100"}},
	})
	got := next.(listTUIModel)
	if !got.moveLookupPending {
		t.Fatalf("stale response should not clear pending state")
	}
	if got.movePicker.active {
		t.Fatalf("stale response should not open move picker")
	}
}
