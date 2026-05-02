package terminal

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/config"
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
	m := listTUIModel{pendingSyncHosts: map[string]struct{}{"": {}}}
	if got := m.emptyStateText(); !strings.Contains(got, "Waiting for startup sync") {
		t.Fatalf("emptyStateText() = %q", got)
	}

	m.pendingSyncHosts = map[string]struct{}{}
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

func TestListTUICountUnplacedQueuedJobsIncludesPendingPlacement(t *testing.T) {
	// Regression: the autopilot gate in runAutoPilot early-returns when this
	// counter is 0. The backend planner accepts both queued and
	// pending_placement unplaced jobs, so the UI counter must too —
	// otherwise a job left in pending_placement (e.g. after an interrupted
	// relaunch pass) hides the autopilot's work item entirely.
	pending := db.StatusPendingPlacement
	m := listTUIModel{
		jobs: []*db.Job{
			{ID: 1, Status: db.StatusQueued},
			{ID: 2, Status: db.StatusQueued, PendingStatus: &pending},
			{ID: 3, Status: db.StatusCompleted},
			{ID: 4, Status: db.StatusQueued, Host: "studio"},
		},
	}
	if got, want := m.countUnplacedQueuedJobs(), 2; got != want {
		t.Fatalf("countUnplacedQueuedJobs() = %d, want %d (should include pending_placement unplaced job)", got, want)
	}
}

func TestListTUIPruneAutoBlockReasonsKeepsOnlyVisibleUnplacedQueued(t *testing.T) {
	m := listTUIModel{
		autoBlockReasons: map[int64]string{
			1: "no offers available",
			2: "no offers available",
			3: "no offers available",
		},
		jobs: []*db.Job{
			{ID: 1, Status: db.StatusQueued},
			{ID: 2, Status: db.StatusQueued, Host: "cool30"},
		},
	}

	m.pruneAutoBlockReasons()

	if got := m.autoBlockReasons[1]; got == "" {
		t.Fatal("expected unplaced queued job to keep block reason")
	}
	if _, ok := m.autoBlockReasons[2]; ok {
		t.Fatal("expected placed queued job block reason to be pruned")
	}
	if _, ok := m.autoBlockReasons[3]; ok {
		t.Fatal("expected non-visible job block reason to be pruned")
	}
}

func TestGroupedJobsWithAutoReasonsOnlyAppliesToUnplacedQueuedJobs(t *testing.T) {
	unplaced := &db.Job{ID: 10, Status: db.StatusQueued}
	placed := &db.Job{ID: 11, Status: db.StatusQueued, Host: "cool30"}
	m := listTUIModel{
		jobs: []*db.Job{unplaced, placed},
		autoBlockReasons: map[int64]string{
			10: "no offers available",
			11: "no offers available",
		},
	}

	decorated := m.groupedJobsWithAutoReasons()
	if len(decorated) != 2 {
		t.Fatalf("decorated len = %d, want 2", len(decorated))
	}

	if decorated[0] == unplaced {
		t.Fatal("expected unplaced job to be copied with injected block reason")
	}
	if got := decorated[0].QueueBlockedReason; got != "no offers available" {
		t.Fatalf("unplaced QueueBlockedReason = %q, want injected reason", got)
	}
	if decorated[1] != placed {
		t.Fatal("expected placed queued job to be returned unchanged")
	}
}

func TestGroupedJobsWithAutoReasons_UnprocessedGroupedViewExcludesCanceledKeepsKilled(t *testing.T) {
	killed := &db.Job{ID: 21, Status: db.StatusKilled}
	canceled := &db.Job{ID: 22, Status: db.StatusCanceled}
	m := listTUIModel{
		groupedByStatus:        true,
		groupedUnprocessedView: true,
		jobs:                   []*db.Job{killed, canceled},
	}

	grouped := m.groupedJobsWithAutoReasons()
	if len(grouped) != 1 {
		t.Fatalf("grouped len = %d, want 1", len(grouped))
	}
	if grouped[0] != killed {
		t.Fatalf("expected killed job to remain, got %#v", grouped[0])
	}
}

func TestListTUIJobsLoadedClearsStalePersistentBlockedSummaryWhenNoUnplacedJobs(t *testing.T) {
	m := listTUIModel{
		autoMode:               true,
		autoPersistentBlocked:  "no offers available",
		autoPersistentBlockedN: 2,
	}

	next, _ := m.Update(listJobsLoadedMsg{
		jobs: []*db.Job{
			{ID: 1, Status: db.StatusQueued, Host: "cool30"},
		},
	})
	got := next.(listTUIModel)

	if got.autoPersistentBlocked != "" {
		t.Fatalf("autoPersistentBlocked = %q, want empty", got.autoPersistentBlocked)
	}
	if got.autoPersistentBlockedN != 0 {
		t.Fatalf("autoPersistentBlockedN = %d, want 0", got.autoPersistentBlockedN)
	}
}

func TestListTUIDBDebounceQueuesTrailingRefresh(t *testing.T) {
	m := listTUIModel{
		debounceActive: true,
	}

	next, _ := m.Update(listDBWatchEventMsg{})
	got := next.(listTUIModel)
	if !got.debounceActive {
		t.Fatal("debounceActive should remain true while debounce window is open")
	}
	if !got.debouncePending {
		t.Fatal("expected debouncePending to be set for trailing refresh")
	}

	next, _ = got.Update(listDBRefreshTriggeredMsg{})
	got = next.(listTUIModel)
	if !got.debounceActive {
		t.Fatal("debounceActive should stay true while trailing refresh is scheduled")
	}
	if got.debouncePending {
		t.Fatal("debouncePending should clear after scheduling trailing refresh")
	}

	next, _ = got.Update(listDBRefreshTriggeredMsg{})
	got = next.(listTUIModel)
	if got.debounceActive {
		t.Fatal("debounceActive should clear when no trailing refresh is pending")
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

func TestListTUIGroupedViewShowsBudgetPromptAfterDollarKey(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		autoMode:        true,
		width:           120,
		height:          24,
		title:           "Jobs",
		jobs: []*db.Job{
			{ID: 1, Status: db.StatusQueued, Description: "queued"},
		},
		autoRunRateTargetCents: 250,
	}
	m.rebuildGroupedRows()

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'$'}})
	got := next.(listTUIModel)
	if !got.autoRunRateInputActive {
		t.Fatal("expected $ to activate the budget input")
	}
	out := stripANSI(got.View())
	if !strings.Contains(out, "Auto-pilot budget") {
		t.Fatalf("expected budget panel header in view after $, got:\n%s", out)
	}
	if !strings.Contains(out, "Hourly target") || !strings.Contains(out, "Daily cap") || !strings.Contains(out, "Reset runaway breaker") {
		t.Fatalf("expected all menu actions visible at once, got:\n%s", out)
	}
	if !strings.Contains(out, "$2.50/hr") {
		t.Fatalf("expected current hourly value in menu, got:\n%s", out)
	}
	if !strings.Contains(out, "Esc") {
		t.Fatalf("expected Esc close hint in view, got:\n%s", out)
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
	if strings.Contains(out, "Auto-pilot failed:") {
		t.Fatalf("expected auto-pilot message to be rendered only in the dedicated auto line, got:\n%s", out)
	}
	if !strings.Contains(out, "A:auto (ON)") {
		t.Fatalf("expected controls line with auto state, got:\n%s", out)
	}
	if !strings.Contains(out, "q:quit") {
		t.Fatalf("expected quit hint in controls line, got:\n%s", out)
	}
	if !strings.Contains(out, "r:refresh") {
		t.Fatalf("expected refresh hint in controls line, got:\n%s", out)
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
	if !strings.Contains(out, "A:auto (ON)") || !strings.Contains(out, "v:ungrou") {
		t.Fatalf("expected controls line to remain visible even with long status, got:\n%s", out)
	}
}

func TestListTUIGroupedStatusTextNormalizesControlCharacters(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		statusMessage:   "peak_rss_kb: n=2373, cv_r2=0.7294\r\new instance\tq:quit",
	}

	got := m.groupedStatusText()
	want := "peak_rss_kb: n=2373, cv_r2=0.7294 | ew instance q:quit"
	if got != want {
		t.Fatalf("groupedStatusText() = %q, want %q", got, want)
	}
}

func TestListTUIRefreshKeySetsRefreshingStatusAndReturnsCommand(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		width:           90,
		height:          12,
		title:           "Jobs",
		jobs: []*db.Job{
			{ID: 733, Status: db.StatusQueued, Description: "retry pending", Project: "proj"},
		},
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")})
	got := next.(listTUIModel)
	if got.statusMessage != "Refreshing..." {
		t.Fatalf("statusMessage = %q, want %q", got.statusMessage, "Refreshing...")
	}
	if cmd == nil {
		t.Fatal("expected refresh key to return a refresh command")
	}
}

func TestListTUIGroupedViewShortViewportPreservesAllSectionHeaders(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		width:           100,
		height:          9,
		title:           "Jobs",
		jobs: []*db.Job{
			{ID: 1, Status: db.StatusRunning, Host: "cool30", Description: "run", Project: "proj"},
			{ID: 2, Status: db.StatusQueued, Host: "cool30", Description: "queue", Project: "proj"},
			{ID: 3, Status: db.StatusCompleted, ExitCode: testIntPtr(0), Description: "done", Project: "proj"},
			{ID: 4, Status: db.StatusFailed, Description: "fail", Project: "proj"},
		},
	}
	m.rebuildGroupedRows()

	out := stripANSI(m.View())
	for _, want := range []string{
		"Running (1)",
		"Queued (1)",
		"Completed (1)",
		"Failed (1)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in grouped short viewport output, got:\n%s", want, out)
		}
	}
}

// TestListTUIGroupedViewShowsSelectedJobDetail verifies the selected-job
// detail footer block (placement + context lines) appears under the job list
// when a job is under the cursor, in both grouped and ungrouped views.
func TestListTUIGroupedViewShowsSelectedJobDetail(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		width:           120,
		height:          30,
		title:           "Jobs",
		jobs: []*db.Job{
			{ID: 42, Status: db.StatusRunning, Host: "cool30", GPU: "0,1", Project: "my-proj", Command: "uv run train.py", StartTime: time.Now().Add(-5 * time.Minute).Unix()},
		},
	}
	m.rebuildGroupedRows()

	out := stripANSI(m.View())
	for _, want := range []string{"Job: wj42", "elapsed 5m", "Host: cool30"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in grouped view output, got:\n%s", want, out)
		}
	}
	// The detail footer must not repeat the project label (already in the
	// job row). "train.py" is legitimately shown in the row itself, so we
	// can't forbid it globally; detail-line-specific exclusions are covered
	// by the unit tests in list_selected_detail_test.go.
	if strings.Contains(out, "project my-proj") {
		t.Fatalf("unexpected project label in grouped view, got:\n%s", out)
	}
}

func TestListTUIUngroupedViewShowsSelectedJobDetail(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: false,
		width:           120,
		height:          30,
		title:           "Jobs",
		layout:          newJobListLayout(120, nil, nil, false),
		jobs: []*db.Job{
			{ID: 7, Status: db.StatusQueued, Host: "", GPUClass: "ampere+", PlacementReasons: []string{"no capacity"}, Project: "bench", CreatedAt: time.Now().Add(-2 * time.Hour).Unix()},
		},
	}

	out := stripANSI(m.View())
	for _, want := range []string{"Job: wj7", "unplaced", "ampere+", "blocked: no capacity", "waiting 2h"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in ungrouped view output, got:\n%s", want, out)
		}
	}
}

// TestListTUIGroupedViewOmitsStandaloneCampaignETA pins a regression: the
// standalone "ETA: …" footer line was removed from the grouped view when
// the per-job ETA moved onto the "Job:" line. The only "ETA " occurrence
// on screen should be the one inside the Job line (prefixed by " · ETA ").
func TestListTUIGroupedViewOmitsStandaloneCampaignETA(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		width:           120,
		height:          30,
		title:           "Jobs",
		jobs: []*db.Job{
			{ID: 1, Status: db.StatusRunning, Host: "cool30", Description: "r", Project: "proj", StartTime: time.Now().Add(-1 * time.Minute).Unix()},
		},
	}
	m.rebuildGroupedRows()

	out := stripANSI(m.View())
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "ETA:") || strings.HasPrefix(trimmed, "ETA ~") {
			t.Fatalf("standalone ETA footer line must be removed; got line:\n%s\n(full output:\n%s)", trimmed, out)
		}
	}
}

func TestSelectGroupedRowsForViewport_EllidesSectionTailWithDots(t *testing.T) {
	rows := buildGroupedStatusRows([]*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: "cool30", Description: "run 1", Project: "proj"},
		{ID: 2, Status: db.StatusRunning, Host: "cool30", Description: "run 2", Project: "proj"},
		{ID: 3, Status: db.StatusRunning, Host: "cool30", Description: "run 3", Project: "proj"},
		{ID: 4, Status: db.StatusRunning, Host: "cool30", Description: "run 4", Project: "proj"},
	}, 120, nil, nil)

	lines := selectGroupedRowsForViewport(rows, 4)
	if len(lines) != 4 {
		t.Fatalf("line count = %d, want 4", len(lines))
	}
	if lines[0].text != "Running (4):" {
		t.Fatalf("first line = %q, want Running header", lines[0].text)
	}
	if lines[3].text != "..." {
		t.Fatalf("last line = %q, want %q", lines[3].text, "...")
	}
}

func TestSelectGroupedRowsForViewport_FullyElidedGroupShowsHeaderWithoutColon(t *testing.T) {
	rows := buildGroupedStatusRows([]*db.Job{
		{ID: 10, Status: db.StatusQueued, Host: "", Description: "u1", Project: "proj"},
		{ID: 11, Status: db.StatusQueued, Host: "", Description: "u2", Project: "proj"},
	}, 120, nil, nil)

	lines := selectGroupedRowsForViewport(rows, 1)
	if len(lines) != 1 {
		t.Fatalf("line count = %d, want 1", len(lines))
	}
	if got := lines[0].text; got != "Unplaced (2)" {
		t.Fatalf("line = %q, want %q", got, "Unplaced (2)")
	}
}

func TestSelectGroupedRowsForViewport_RemovesBlankLinesBetweenAbbreviatedGroups(t *testing.T) {
	rows := buildGroupedStatusRows([]*db.Job{
		{ID: 20, Status: db.StatusRunning, Host: "cool30", Description: "r1", Project: "proj"},
		{ID: 21, Status: db.StatusRunning, Host: "cool30", Description: "r2", Project: "proj"},
		{ID: 30, Status: db.StatusQueued, Host: "", Description: "u1", Project: "proj"},
		{ID: 31, Status: db.StatusQueued, Host: "", Description: "u2", Project: "proj"},
	}, 120, nil, nil)

	lines := selectGroupedRowsForViewport(rows, 3)
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2", len(lines))
	}
	if strings.TrimSpace(lines[0].text) == "" || strings.TrimSpace(lines[1].text) == "" {
		t.Fatalf("expected no blank separator between abbreviated groups, got: %+v", lines)
	}
}

func TestListTUIGroupedViewPinsFooterAtBottomWithSeparator(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		width:           80,
		height:          12,
		title:           "Jobs",
		jobs:            nil,
	}
	m.rebuildGroupedRows()

	out := stripANSI(m.View())
	lines := strings.Split(out, "\n")
	if len(lines) != m.height {
		t.Fatalf("rendered line count = %d, want %d\n%s", len(lines), m.height, out)
	}
	if !strings.Contains(lines[len(lines)-1], "q:quit") {
		t.Fatalf("last line = %q, want controls footer", lines[len(lines)-1])
	}
	if strings.TrimSpace(lines[len(lines)-2]) != "" {
		t.Fatalf("expected blank separator above footer, got %q", lines[len(lines)-2])
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

	// Seed shared-status cache: View() is async-refresh and would otherwise
	// render an empty system line on first call.
	sharedTUIStatusCache.mu.Lock()
	sharedTUIStatusCache.expires = time.Time{}
	sharedTUIStatusCache.initialized = false
	sharedTUIStatusCache.mu.Unlock()
	refreshSharedTUIStatus(database)

	out := stripANSI(m.View())
	sharedIdx := strings.Index(out, "0 jobs running")
	statusIdx := strings.Index(out, "Auto-pilot: monitoring")
	controlsIdx := strings.Index(out, "A:auto (ON)")
	if sharedIdx < 0 || statusIdx < 0 || controlsIdx < 0 {
		t.Fatalf("missing grouped footer parts, got:\n%s", out)
	}
	if !(sharedIdx < statusIdx && statusIdx < controlsIdx) {
		t.Fatalf("expected shared status -> status -> controls order, got:\n%s", out)
	}
}

func TestListTUIAutoPilotFailureSchedulesCooldown(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		autoMode:        true,
		autoInProgress:  true,
		database:        &sql.DB{},
		jobs: []*db.Job{
			{ID: 1, Status: db.StatusQueued, Tags: []string{"rental"}},
		},
	}

	next, _ := m.Update(listAutoPilotDoneMsg{err: errors.New("boom")})
	got := next.(listTUIModel)
	if got.autoInProgress {
		t.Fatal("autoInProgress should be cleared after autopilot failure")
	}
	if !strings.Contains(got.statusMessage, "Auto-pilot failed: boom") {
		t.Fatalf("statusMessage = %q, want failure text", got.statusMessage)
	}
	if got.autoNextPassAt.IsZero() || time.Until(got.autoNextPassAt) <= 0 {
		t.Fatalf("expected cooldown after failure, got autoNextPassAt=%v", got.autoNextPassAt)
	}

	next2, _ := got.Update(listSyncTickMsg{})
	got2 := next2.(listTUIModel)
	if got2.autoInProgress {
		t.Fatal("expected cooldown to suppress immediate retry after failure")
	}

	got2.autoNextPassAt = time.Now().Add(-time.Second)
	next3, _ := got2.Update(listSyncTickMsg{})
	got3 := next3.(listTUIModel)
	if !got3.autoInProgress {
		t.Fatal("expected autopilot to re-run once cooldown elapses")
	}
}

func TestListTUIAutoPilotOutcomeCooldowns(t *testing.T) {
	cases := []struct {
		name string
		msg  listAutoPilotDoneMsg
		want time.Duration
	}{
		{"progress", listAutoPilotDoneMsg{launched: 1}, listAutoPilotCooldownProgress},
		{"blocked", listAutoPilotDoneMsg{blockedReasons: map[int64]string{1: "waiting"}}, listAutoPilotCooldownBlocked},
		{"idle", listAutoPilotDoneMsg{}, listAutoPilotCooldownIdle},
		{"contention", listAutoPilotDoneMsg{anotherHolding: true}, listAutoPilotCooldownContend},
		{"error", listAutoPilotDoneMsg{err: errors.New("boom")}, listAutoPilotCooldownError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := listTUIModel{
				groupedByStatus: true,
				autoMode:        true,
				autoInProgress:  true,
				database:        &sql.DB{},
			}
			start := time.Now()
			next, _ := m.Update(tc.msg)
			got := next.(listTUIModel)
			if got.autoNextPassAt.IsZero() {
				t.Fatalf("%s: autoNextPassAt not set", tc.name)
			}
			delta := got.autoNextPassAt.Sub(start)
			// Allow a small scheduling tolerance around the expected duration.
			if delta < tc.want-time.Second || delta > tc.want+time.Second {
				t.Fatalf("%s: cooldown = %v, want ~%v", tc.name, delta, tc.want)
			}
		})
	}
}

func TestListTUIResumeAutoPilotNowClearsCooldown(t *testing.T) {
	makeModel := func() listTUIModel {
		return listTUIModel{
			groupedByStatus: true,
			autoMode:        true,
			database:        &sql.DB{},
			autoNextPassAt:  time.Now().Add(30 * time.Second),
		}
	}

	t.Run("toggle auto on", func(t *testing.T) {
		m := makeModel()
		m.autoMode = false
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
		got := next.(listTUIModel)
		if !got.autoMode {
			t.Fatal("expected autoMode ON after 'A' toggle")
		}
		if !got.autoNextPassAt.IsZero() {
			t.Fatalf("expected cooldown cleared, got %v", got.autoNextPassAt)
		}
	})

	t.Run("manual refresh", func(t *testing.T) {
		m := makeModel()
		next, _ := m.triggerManualRefresh()
		if !next.autoNextPassAt.IsZero() {
			t.Fatalf("expected cooldown cleared after refresh, got %v", next.autoNextPassAt)
		}
	})

	t.Run("budget editor save clears cooldown", func(t *testing.T) {
		// Saving an edited value should retrigger the autopilot, which clears
		// the cooldown timestamp. Drive the menu → editor → save flow.
		m := makeModel()
		m.autoRunRateInputActive = true
		m.autoRunRateInputPhase = autoBudgetPhaseEditHourly
		m.autoRunRateInputValue = "3.50"
		next, _ := m.handleAutoRunRateInputKey(tea.KeyMsg{Type: tea.KeyEnter})
		got := next.(listTUIModel)
		if got.autoRunRateInputPhase != autoBudgetPhaseMenu {
			t.Fatalf("after save expected menu phase, got %v", got.autoRunRateInputPhase)
		}
		if !got.autoRunRateInputActive {
			t.Fatal("save should not close the panel; should return to menu")
		}
		if !got.autoNextPassAt.IsZero() {
			t.Fatalf("expected cooldown cleared after save, got %v", got.autoNextPassAt)
		}
	})
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

func TestNormalizeStatusLineTextCollapsesMultilineIndentedText(t *testing.T) {
	raw := "Error: app osteele-weft-builder has no started VMs.\n       It may be unhealthy or not have been deployed yet."
	got := normalizeStatusLineText(raw)
	want := "Error: app osteele-weft-builder has no started VMs. | It may be unhealthy or not have been deployed yet."
	if got != want {
		t.Fatalf("normalizeStatusLineText() = %q, want %q", got, want)
	}
}

func TestListTUIAutoPilotFailureStoresRawErrorAndShowsNormalizedStatus(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		autoMode:        true,
		autoInProgress:  true,
		database:        &sql.DB{},
	}
	err := errors.New("line one\n    line two")
	next, _ := m.Update(listAutoPilotDoneMsg{err: err})
	got := next.(listTUIModel)

	if got.lastAutoPilotErrorRaw != "line one\n    line two" {
		t.Fatalf("lastAutoPilotErrorRaw = %q", got.lastAutoPilotErrorRaw)
	}
	if strings.Contains(got.statusMessage, "\n") {
		t.Fatalf("statusMessage should be single-line, got %q", got.statusMessage)
	}
	if !strings.Contains(got.statusMessage, "line one | line two") {
		t.Fatalf("statusMessage = %q, want normalized summary", got.statusMessage)
	}
}

func TestListTUIRunAutoPilotKeepsSessionRunRateTarget(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[campaign]\nauto_run_rate_soft_target = 0.5\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restore := config.SetConfigPathsForTesting(cfgPath, filepath.Join(dir, "config.yaml"))
	defer restore()

	m := listTUIModel{
		groupedByStatus:        false, // early-return path is enough to verify target stability.
		autoMode:               false,
		database:               nil,
		autoRunRateTargetCents: 250,
	}

	_ = m.runAutoPilot()
	if m.autoRunRateTargetCents != 250 {
		t.Fatalf("autoRunRateTargetCents = %d, want 250", m.autoRunRateTargetCents)
	}
}

func TestListTUIGroupedKeyETogglesErrorDetailsPanel(t *testing.T) {
	m := listTUIModel{
		groupedByStatus:       true,
		width:                 100,
		height:                20,
		lastAutoPilotErrorRaw: "line one\n  line two",
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	got := next.(listTUIModel)
	if !got.showAutoPilotErrorDetails {
		t.Fatal("expected error details panel to be enabled")
	}
	out := stripANSI(got.View())
	if !strings.Contains(out, "Auto-pilot error details:") {
		t.Fatalf("expected details header in view, got:\n%s", out)
	}
	if !strings.Contains(out, "line one") || !strings.Contains(out, "  line two") {
		t.Fatalf("expected raw multiline error text in details panel, got:\n%s", out)
	}
	if !strings.Contains(out, "e:hide error") {
		t.Fatalf("expected controls hint to hide details, got:\n%s", out)
	}

	next2, _ := got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	got2 := next2.(listTUIModel)
	if got2.showAutoPilotErrorDetails {
		t.Fatal("expected error details panel to be disabled")
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
	if !strings.Contains(line, "k:kill") || !strings.Contains(line, "u:unplace") || !strings.Contains(line, "p:processed") || !strings.Contains(line, "m:move") {
		t.Fatalf("controls line missing queued-job actions: %q", line)
	}
}

func TestListTUIKeyV_TogglesBetweenGroupedAndUngrouped(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: false,
		width:           100,
		height:          20,
		jobs: []*db.Job{
			{ID: 1, Status: db.StatusQueued, Description: "queued"},
		},
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	got := next.(listTUIModel)
	if !got.groupedByStatus {
		t.Fatal("expected groupedByStatus=true after pressing v from ungrouped list")
	}
	if got.statusMessage != "Grouped status view" {
		t.Fatalf("statusMessage = %q, want %q", got.statusMessage, "Grouped status view")
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	got = next.(listTUIModel)
	if got.groupedByStatus {
		t.Fatal("expected groupedByStatus=false after pressing v from grouped list")
	}
	if got.statusMessage != "Ungrouped list view" {
		t.Fatalf("statusMessage = %q, want %q", got.statusMessage, "Ungrouped list view")
	}
}

func TestListTUIKeyI_RequestsSwitchToSystemWatch(t *testing.T) {
	m := listTUIModel{groupedByStatus: true}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}})
	_ = next.(listTUIModel)
	if cmd == nil {
		t.Fatal("expected switch command")
	}
	msg := cmd()
	if _, ok := msg.(switchToSystemWatchMsg); !ok {
		t.Fatalf("expected switchToSystemWatchMsg, got %T", msg)
	}
}

func TestListTUIGroupedKeyPMarksSelectedJobProcessed(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	if job == nil {
		t.Fatalf("job %d not found", jobID)
	}

	m := listTUIModel{
		database:        database,
		groupedByStatus: true,
		width:           100,
		height:          20,
		jobs:            []*db.Job{job},
	}
	m.rebuildGroupedRows()

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	got := next.(listTUIModel)
	if cmd == nil {
		t.Fatal("expected command to mark selected job as processed")
	}
	if !strings.Contains(got.statusMessage, "Marking job #") {
		t.Fatalf("statusMessage = %q, want marking text", got.statusMessage)
	}

	msg := cmd()
	next2, reloadCmd := got.Update(msg)
	got2 := next2.(listTUIModel)
	if reloadCmd == nil {
		t.Fatal("expected reload command after mark processed completion")
	}
	if !strings.Contains(got2.statusMessage, "marked as processed") {
		t.Fatalf("statusMessage = %q, want processed confirmation", got2.statusMessage)
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID updated: %v", err)
	}
	if updated == nil || !updated.HasTag(db.ProcessedTag) {
		t.Fatalf("expected job %d to have processed tag", jobID)
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

func TestListTUIHelpOverlayOpensAndClosesInUngroupedView(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: false,
		width:           100,
		height:          20,
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	got := next.(listTUIModel)
	if !got.showHelp {
		t.Fatal("expected help overlay to open")
	}
	out := stripANSI(got.View())
	if !strings.Contains(out, "Jobs List Keybindings") {
		t.Fatalf("expected list help title, got:\n%s", out)
	}
	if !strings.Contains(out, "v toggle grouped/ungrouped view") {
		t.Fatalf("expected shared keybinding help text, got:\n%s", out)
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got = next.(listTUIModel)
	if got.showHelp {
		t.Fatal("expected help overlay to close on Esc")
	}
}

func TestListTUIHelpOverlayShowsGroupedActions(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		width:           100,
		height:          20,
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	got := next.(listTUIModel)
	if !got.showHelp {
		t.Fatal("expected grouped help overlay to open")
	}
	out := stripANSI(got.View())
	if !strings.Contains(out, "Grouped-only actions:") {
		t.Fatalf("expected grouped actions section, got:\n%s", out)
	}
	if !strings.Contains(out, "m move selected queued job") {
		t.Fatalf("expected grouped move keybinding, got:\n%s", out)
	}
	if !strings.Contains(out, "e toggle auto-pilot error details") {
		t.Fatalf("expected grouped error-details keybinding, got:\n%s", out)
	}
}
