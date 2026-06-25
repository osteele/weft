package terminal

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/degraded"
	"github.com/osteele/weft/internal/jobview"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/orchestration"
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

func TestRenderJobListPlainShowsProjectWithoutDirectoryOnWideTerminals(t *testing.T) {
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
	if strings.Contains(out, "DIR") {
		t.Fatalf("output unexpectedly includes DIR header, got:\n%s", out)
	}
	if !strings.Contains(out, "PROJECT") {
		t.Fatalf("output missing PROJECT header, got:\n%s", out)
	}
	if !strings.Contains(out, "llm-performance-models") {
		t.Fatalf("output missing project name, got:\n%s", out)
	}
}

func TestRenderJobListPlainUsesTerminalTimeForTerminalJobs(t *testing.T) {
	end := time.Date(2026, 5, 10, 8, 30, 0, 0, time.Local).Unix()
	jobs := []*db.Job{
		{
			ID:          42,
			Status:      db.StatusCompleted,
			StartTime:   end - 3600,
			EndTime:     &end,
			Project:     "proj",
			Description: "train model",
		},
	}

	out := renderJobListPlain(jobs, 120)
	if !strings.Contains(out, "TIME") {
		t.Fatalf("output missing TIME header, got:\n%s", out)
	}
	if !strings.Contains(out, "05/10 08:30") {
		t.Fatalf("output missing terminal time, got:\n%s", out)
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

func TestListTUIGroupedUnprocessedHidesCanceledJobs(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusCanceled, Description: "handled cancel"},
		{ID: 2, Status: db.StatusKilled, Description: "needs review"},
	}
	m := listTUIModel{
		groupedByStatus: true,
		unprocessedView: true,
		width:           100,
		height:          20,
		title:           "Jobs",
	}

	next, _ := m.Update(listJobsLoadedMsg{jobs: jobs})
	got := next.(listTUIModel)

	if len(got.jobs) != 1 || got.jobs[0].ID != 2 {
		t.Fatalf("visible jobs = %#v, want only killed job", got.jobs)
	}
	out := stripANSI(got.View())
	if !strings.Contains(out, "Jobs • unprocessed (1)") {
		t.Fatalf("title count should ignore hidden canceled jobs, got:\n%s", out)
	}
	if strings.Contains(out, "handled cancel") {
		t.Fatalf("grouped unprocessed view should hide canceled jobs, got:\n%s", out)
	}

	flat := listTUIModel{unprocessedView: true}
	next, _ = flat.Update(listJobsLoadedMsg{jobs: jobs})
	flatGot := next.(listTUIModel)
	if len(flatGot.jobs) != 2 {
		t.Fatalf("flat unprocessed jobs = %d, want 2", len(flatGot.jobs))
	}
}

func TestListTUIToggleStatusAreaIncreasesBodyRows(t *testing.T) {
	m := listTUIModel{
		width:  100,
		height: 12,
		jobs: []*db.Job{
			{ID: 1, Host: "studio", Status: db.StatusRunning, Description: "job"},
		},
	}
	m.rebuildLayout()

	before := m.flatBodyRows()
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("S")})
	got := next.(listTUIModel)
	if !got.hideStatusArea {
		t.Fatal("expected status area to be hidden")
	}
	if after := got.flatBodyRows(); after <= before {
		t.Fatalf("body rows after hiding status = %d, want > %d", after, before)
	}
}

func TestListTUIProjectFilterPromptAppliesFilter(t *testing.T) {
	oldDeps := deps
	t.Cleanup(func() { deps = oldDeps })

	var gotProject string
	deps.CollectJobsForListWithFilters = func(_ *sql.DB, _ []string, _, _, projectFilter string) ([]*db.Job, error) {
		gotProject = projectFilter
		if projectFilter == "" {
			return []*db.Job{
				{ID: 40, Host: "studio", Status: db.StatusCompleted, Project: "alpha", Description: "alpha"},
				{ID: 41, Host: "studio", Status: db.StatusCompleted, Project: "alpine", Description: "alpine"},
				{ID: 42, Host: "studio", Status: db.StatusCompleted, Project: "beta", Description: "beta"},
			}, nil
		}
		return []*db.Job{{ID: 42, Host: "studio", Status: db.StatusCompleted, Project: projectFilter, Description: "job"}}, nil
	}

	m := listTUIModel{database: db.SetupTestDB(t), width: 100, height: 12}
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m = next.(listTUIModel)
	if cmd == nil {
		t.Fatal("expected candidate load command")
	}
	next, _ = m.Update(cmd())
	m = next.(listTUIModel)
	for _, r := range "alp" {
		next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = next.(listTUIModel)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(listTUIModel)
	next, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(listTUIModel)
	if !strings.Contains(m.statusMessage, "alpine") {
		t.Fatalf("statusMessage = %q, want project name", m.statusMessage)
	}
	if cmd == nil {
		t.Fatal("expected reload command")
	}
	msg := cmd()
	next, _ = m.Update(msg)
	m = next.(listTUIModel)
	if gotProject != "alpine" {
		t.Fatalf("project filter = %q, want alpine", gotProject)
	}
	if len(m.jobs) != 1 || m.jobs[0].Project != "alpine" {
		t.Fatalf("jobs = %+v, want filtered project job", m.jobs)
	}
}

func TestListTUIPageDownScrollsFlatViewport(t *testing.T) {
	jobs := make([]*db.Job, 20)
	for i := range jobs {
		jobs[i] = &db.Job{ID: int64(i + 1), Host: "studio", Status: db.StatusQueued, Description: "job"}
	}
	m := listTUIModel{
		width:  100,
		height: 12,
		jobs:   jobs,
	}
	m.rebuildLayout()
	bodyRows := m.flatBodyRows()
	if bodyRows <= 1 {
		t.Fatalf("bodyRows = %d, want enough rows for paging", bodyRows)
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	got := next.(listTUIModel)
	if got.cursor != bodyRows {
		t.Fatalf("cursor after page down = %d, want %d", got.cursor, bodyRows)
	}
	if got.offset == 0 {
		t.Fatalf("offset after page down = %d, want viewport to scroll", got.offset)
	}
	if got.cursor < got.offset || got.cursor >= got.offset+bodyRows {
		t.Fatalf("cursor %d not visible in offset %d bodyRows %d", got.cursor, got.offset, bodyRows)
	}
}

func TestListHostSyncRequestPromotesUnsyncedQueuedInventoryJobs(t *testing.T) {
	req := listHostSyncRequest("cool30", []*db.Job{
		{ID: 1811, Host: "cool30", Status: db.StatusQueued},
	})

	if req.Mode != ops.SyncModeFull {
		t.Fatalf("request mode = %q, want %q", req.Mode, ops.SyncModeFull)
	}
}

func TestListHostSyncRequestKeepsStatusModeForRecentlyDispatchedQueuedJobs(t *testing.T) {
	req := listHostSyncRequest("cool30", []*db.Job{
		{ID: 1811, Host: "cool30", Status: db.StatusQueued, LastSyncedStatus: db.StatusQueued, QueuedAt: time.Now().Unix()},
	})

	if req.Mode != ops.SyncModeStatus {
		t.Fatalf("request mode = %q, want %q", req.Mode, ops.SyncModeStatus)
	}
}

func TestListHostSyncRequestPromotesStaleDispatchedQueuedJobs(t *testing.T) {
	req := listHostSyncRequest("cool30", []*db.Job{
		{
			ID:               1811,
			Host:             "cool30",
			Status:           db.StatusQueued,
			LastSyncedStatus: db.StatusQueued,
			QueuedAt:         time.Now().Add(-time.Minute).Unix(),
		},
	})

	if req.Mode != ops.SyncModeFull {
		t.Fatalf("request mode = %q, want %q", req.Mode, ops.SyncModeFull)
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

func TestListTUICountAutoPilotActionableQueuedJobsIncludesRentalQueue(t *testing.T) {
	pending := db.StatusPendingPlacement
	launchID := int64(3648)
	m := listTUIModel{
		jobs: []*db.Job{
			{ID: 1, Status: db.StatusQueued},
			{ID: 2, Status: db.StatusQueued, PendingStatus: &pending},
			{ID: 3, Status: db.StatusQueued, LaunchID: &launchID},
			{ID: 4, Status: db.StatusRunning, LaunchID: &launchID},
			{ID: 5, Status: db.StatusQueued, Host: "studio"},
		},
	}
	if got, want := m.countAutoPilotActionableQueuedJobs(), 3; got != want {
		t.Fatalf("countAutoPilotActionableQueuedJobs() = %d, want %d", got, want)
	}
}

func TestListTUIAutopilotCountsExcludeOpenMoveIntentUnplacedJobs(t *testing.T) {
	m := listTUIModel{
		jobs: []*db.Job{
			{ID: 1, Status: db.StatusQueued},
			{ID: 2, Status: db.StatusQueued},
		},
		placementStatusByJob: map[int64]jobview.PlacementStatus{
			2: {
				JobID: 2,
				Move:  &jobview.MoveDisplay{State: db.MoveIntentStateOpen},
			},
		},
	}

	if got, want := m.countUnplacedQueuedJobs(), 1; got != want {
		t.Fatalf("countUnplacedQueuedJobs() = %d, want %d", got, want)
	}
	if got, want := m.countAutoPilotActionableQueuedJobs(), 1; got != want {
		t.Fatalf("countAutoPilotActionableQueuedJobs() = %d, want %d", got, want)
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

func TestListTUIPruneAutoBlockReasonsDropsOpenMoveIntentUnplacedJob(t *testing.T) {
	m := listTUIModel{
		autoBlockReasons: map[int64]string{
			1: "run-rate headroom exhausted",
		},
		jobs: []*db.Job{{ID: 1, Status: db.StatusQueued}},
		placementStatusByJob: map[int64]jobview.PlacementStatus{
			1: {
				JobID: 1,
				Move:  &jobview.MoveDisplay{State: db.MoveIntentStateOpen},
			},
		},
	}

	m.pruneAutoBlockReasons()

	if _, ok := m.autoBlockReasons[1]; ok {
		t.Fatalf("expected open-move job block reason to be pruned, got %v", m.autoBlockReasons)
	}
}

func TestListTUIPruneAutoBlockReasonsKeepsCloudAssignedQueued(t *testing.T) {
	launchID := int64(42)
	m := listTUIModel{
		autoBlockReasons: map[int64]string{
			1: "cool100 overloaded; cool30 driver too old",
		},
		jobs: []*db.Job{
			// Job queued on a cloud instance (has LaunchID) but not yet running.
			{ID: 1, Status: db.StatusQueued, LaunchID: &launchID},
		},
	}

	m.pruneAutoBlockReasons()

	if got := m.autoBlockReasons[1]; got == "" {
		t.Fatal("expected cloud-assigned queued job to keep block reason during transient window")
	}
}

func TestListTUIPruneAutoBlockReasonsDropsStaleRunRateHeadroom(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		CostPerHourCents: 60,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	m := listTUIModel{
		database:               database,
		autoRunRateTargetCents: 350,
		autoPersistentBlocked:  "run-rate headroom exhausted ($1.51/hr free, this group needs $2.29/hr)",
		autoPersistentBlockedN: 1,
		autoBlockReasons: map[int64]string{
			1: "run-rate headroom exhausted ($1.51/hr free, this group needs $2.29/hr)",
		},
		jobs: []*db.Job{{ID: 1, Status: db.StatusQueued}},
	}

	m.pruneAutoBlockReasons()

	if len(m.autoBlockReasons) != 0 {
		t.Fatalf("autoBlockReasons = %v, want stale run-rate reason pruned", m.autoBlockReasons)
	}
	if m.autoPersistentBlocked != "" || m.autoPersistentBlockedN != 0 {
		t.Fatalf("persistent blocked = %q/%d, want cleared", m.autoPersistentBlocked, m.autoPersistentBlockedN)
	}
}

func TestListTUIPruneAutoBlockReasonsDropsStaleRunRateSegment(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		CostPerHourCents: 40,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	reason := "run-rate headroom exhausted ($0.41/hr free, this group needs $2.69/hr); reuse blocked: wi2814 RTX 3090 24GB: GPU class mismatch"
	m := listTUIModel{
		database:               database,
		autoRunRateTargetCents: 350,
		autoPersistentBlocked:  reason,
		autoPersistentBlockedN: 1,
		autoBlockReasons:       map[int64]string{1: reason},
		jobs:                   []*db.Job{{ID: 1, Status: db.StatusQueued}},
	}

	m.pruneAutoBlockReasons()

	if len(m.autoBlockReasons) != 0 {
		t.Fatalf("autoBlockReasons = %v, want reuse-only blocker pruned", m.autoBlockReasons)
	}
	if m.autoPersistentBlocked != "" || m.autoPersistentBlockedN != 0 {
		t.Fatalf("persistent blocked = %q/%d, want cleared", m.autoPersistentBlocked, m.autoPersistentBlockedN)
	}
}

func TestListTUIPruneAutoBlockReasonsDropsReuseOnlyDiagnostic(t *testing.T) {
	reason := "could not reuse running instances: wi3816 RTX A6000 45GB: GPU memory insufficient: job=48GB"
	m := listTUIModel{
		autoPersistentBlocked:  reason,
		autoPersistentBlockedN: 1,
		autoBlockReasons:       map[int64]string{1: reason},
		jobs:                   []*db.Job{{ID: 1, Status: db.StatusQueued}},
	}

	m.pruneAutoBlockReasons()

	if len(m.autoBlockReasons) != 0 {
		t.Fatalf("autoBlockReasons = %v, want reuse-only blocker pruned", m.autoBlockReasons)
	}
	if m.autoPersistentBlocked != "" || m.autoPersistentBlockedN != 0 {
		t.Fatalf("persistent blocked = %q/%d, want cleared", m.autoPersistentBlocked, m.autoPersistentBlockedN)
	}
}

func TestListTUIPruneAutoBlockReasonsKeepsCurrentRunRateHeadroom(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		CostPerHourCents: 199,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	reason := "run-rate headroom exhausted ($1.51/hr free, this group needs $2.29/hr)"
	m := listTUIModel{
		database:               database,
		autoRunRateTargetCents: 350,
		autoPersistentBlocked:  reason,
		autoPersistentBlockedN: 1,
		autoBlockReasons:       map[int64]string{1: reason},
		jobs:                   []*db.Job{{ID: 1, Status: db.StatusQueued}},
	}

	m.pruneAutoBlockReasons()

	if got := m.autoBlockReasons[1]; got != reason {
		t.Fatalf("autoBlockReasons[1] = %q, want %q", got, reason)
	}
	if m.autoPersistentBlocked == "" || m.autoPersistentBlockedN != 1 {
		t.Fatalf("persistent blocked = %q/%d, want preserved", m.autoPersistentBlocked, m.autoPersistentBlockedN)
	}
}

func TestListTUIPrunesPersistedStaleRunRateHeadroom(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		CostPerHourCents: 45,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	reason := "run-rate headroom exhausted ($0.53/hr free, this group needs $1.17/hr)"
	if err := db.SetJobPlacementReasons(database, jobID, []string{reason}); err != nil {
		t.Fatalf("SetJobPlacementReasons: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	m := listTUIModel{
		database:               database,
		autoRunRateTargetCents: 350,
	}

	reasons := m.visibleUnplacedBlockedReasonsForJobs([]*db.Job{job})

	if got := reasons[jobID]; got != "" {
		t.Fatalf("visible reason = %q, want none", got)
	}
	refreshed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID refreshed: %v", err)
	}
	if len(refreshed.PlacementReasons) != 1 || refreshed.PlacementReasons[0] != reason {
		t.Fatalf("placement reasons mutated during display: %#v", refreshed.PlacementReasons)
	}
}

func TestListTUIPrunesPersistedStaleRunRateSegment(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		CostPerHourCents: 45,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	reuseReason := "reuse blocked: wi3020 A40 45GB: GPU memory insufficient: job=48GB instance=45GB"
	reason := "run-rate headroom exhausted ($0.53/hr free, this group needs $1.17/hr); " + reuseReason
	if err := db.SetJobPlacementReasons(database, jobID, []string{reason}); err != nil {
		t.Fatalf("SetJobPlacementReasons: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	m := listTUIModel{
		database:               database,
		autoRunRateTargetCents: 350,
	}

	reasons := m.visibleUnplacedBlockedReasonsForJobs([]*db.Job{job})

	if got := reasons[jobID]; got != "" {
		t.Fatalf("visible reason = %q, want none", got)
	}
	refreshed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID refreshed: %v", err)
	}
	if len(refreshed.PlacementReasons) != 1 || refreshed.PlacementReasons[0] != reason {
		t.Fatalf("placement reasons mutated during display: %#v", refreshed.PlacementReasons)
	}
}

func TestListTUIKeepsPersistedCurrentRunRateHeadroom(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		CostPerHourCents: 300,
	}); err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	reason := "run-rate headroom exhausted ($0.53/hr free, this group needs $1.17/hr)"
	if err := db.SetJobPlacementReasons(database, jobID, []string{reason}); err != nil {
		t.Fatalf("SetJobPlacementReasons: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	m := listTUIModel{
		database:               database,
		autoRunRateTargetCents: 350,
	}

	reasons := m.visibleUnplacedBlockedReasonsForJobs([]*db.Job{job})

	if got := reasons[jobID]; got != reason {
		t.Fatalf("visible reason = %q, want %q", got, reason)
	}
	refreshed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID refreshed: %v", err)
	}
	if len(refreshed.PlacementReasons) != 1 || refreshed.PlacementReasons[0] != reason {
		t.Fatalf("placement reasons = %#v, want [%q]", refreshed.PlacementReasons, reason)
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

func TestGroupedJobsWithAutoReasonsSkipsOpenMoveIntentJobs(t *testing.T) {
	moving := &db.Job{ID: 10, Status: db.StatusQueued}
	m := listTUIModel{
		jobs: []*db.Job{moving},
		autoBlockReasons: map[int64]string{
			10: "run-rate headroom exhausted",
		},
		placementStatusByJob: map[int64]jobview.PlacementStatus{
			10: {
				JobID: 10,
				Move:  &jobview.MoveDisplay{State: db.MoveIntentStateOpen},
			},
		},
	}

	decorated := m.groupedJobsWithAutoReasons()
	if len(decorated) != 1 {
		t.Fatalf("decorated len = %d, want 1", len(decorated))
	}
	if decorated[0] != moving {
		t.Fatalf("expected moving job to remain undecorated, got %#v", decorated[0])
	}
	if line := m.groupedAutoPilotStatusText(0); strings.Contains(line, "1 unplaced") {
		t.Fatalf("auto-pilot line = %q, want open-move job excluded from unplaced count", line)
	}
}

func TestGroupedJobsWithAutoReasonsUsesPersistedPlacementReason(t *testing.T) {
	unplaced := &db.Job{
		ID:               10,
		Status:           db.StatusQueued,
		PlacementReasons: []string{"older reason", "planner: no offers from providers for gpu=A100 vram>=82GB"},
	}
	m := listTUIModel{
		groupedByStatus: true,
		jobs:            []*db.Job{unplaced},
	}

	decorated := m.groupedJobsWithAutoReasons()
	if len(decorated) != 1 {
		t.Fatalf("decorated len = %d, want 1", len(decorated))
	}
	if decorated[0] == unplaced {
		t.Fatal("expected unplaced job to be copied with persisted block reason")
	}
	if got := decorated[0].QueueBlockedReason; got != "planner: no offers from providers for gpu=A100 vram>=82GB" {
		t.Fatalf("QueueBlockedReason = %q", got)
	}

	line := m.groupedAutoPilotStatusText(0)
	if !strings.Contains(line, "Auto-pilot: blocked") || !strings.Contains(line, "no offers from providers") {
		t.Fatalf("auto-pilot line = %q, want persisted block reason", line)
	}
}

func TestGroupedAutoPilotStatusTextCountsOnlyBlockedReasons(t *testing.T) {
	jobs := []*db.Job{
		{ID: 3342, Status: db.StatusQueued},
		{ID: 3344, Status: db.StatusQueued},
		{ID: 3345, Status: db.StatusQueued},
	}
	m := listTUIModel{
		groupedByStatus: true,
		jobs:            jobs,
		autoBlockReasons: map[int64]string{
			3342: `job wj3342 declares input "checkpoint:role-enc/exp036-klm3-s42", which weft cannot resolve`,
			3344: "autopilot placing jobs",
			3345: "autopilot placing jobs",
		},
	}

	line := m.groupedAutoPilotStatusText(0)

	if !strings.Contains(line, "Auto-pilot: blocked") {
		t.Fatalf("auto-pilot line = %q, want blocked for the real blocker", line)
	}
	if !strings.Contains(line, "(1 jobs)") {
		t.Fatalf("auto-pilot line = %q, want filtered blocked count", line)
	}
	if strings.Contains(line, "3 jobs blocked") {
		t.Fatalf("auto-pilot line = %q, must not count waiting jobs as blocked", line)
	}
}

func TestGroupedAutoPilotStatusTextDoesNotBlockOnWaitOnlyReasons(t *testing.T) {
	jobs := []*db.Job{
		{ID: 3344, Status: db.StatusQueued},
		{ID: 3345, Status: db.StatusQueued},
	}
	m := listTUIModel{
		groupedByStatus: true,
		jobs:            jobs,
		autoBlockReasons: map[int64]string{
			3344: "autopilot placing jobs",
			3345: "placement pending",
		},
	}

	line := m.groupedAutoPilotStatusText(0)

	if strings.Contains(line, "blocked") {
		t.Fatalf("auto-pilot line = %q, wait-only reasons must not render as blocked", line)
	}
	if !strings.Contains(line, "monitoring") {
		t.Fatalf("auto-pilot line = %q, want monitoring fallback", line)
	}
}

func TestGroupedJobsWithAutoReasonsHidesOnPremOnlyAutoReasonForRentalEligibleJob(t *testing.T) {
	unplaced := &db.Job{
		ID:     10,
		Status: db.StatusQueued,
	}
	m := listTUIModel{
		groupedByStatus: true,
		jobs:            []*db.Job{unplaced},
		autoBlockReasons: map[int64]string{
			10: "3 hosts: host is opt-in only (specify with --host)",
		},
	}

	decorated := m.groupedJobsWithAutoReasons()
	if len(decorated) != 1 {
		t.Fatalf("decorated len = %d, want 1", len(decorated))
	}
	if decorated[0] != unplaced {
		t.Fatalf("expected unplaced job to remain undecorated, got %#v", decorated[0])
	}

	line := m.groupedAutoPilotStatusText(0)
	if strings.Contains(line, "blocked") {
		t.Fatalf("auto-pilot line = %q, want no transient local-host block", line)
	}
}

func TestGroupedAutoPilotStatusTextShowsDisabledState(t *testing.T) {
	m := listTUIModel{
		groupedByStatus:       true,
		autopilotPaused:       true,
		autopilotPausedReason: "manual relaunch",
		autoInProgress:        true,
		jobs:                  []*db.Job{{ID: 10, Status: db.StatusQueued}},
	}

	line := m.groupedAutoPilotStatusText(0)
	if !strings.Contains(line, "off") {
		t.Fatalf("auto-pilot line = %q, want off state", line)
	}
	if !strings.Contains(line, "manual relaunch") {
		t.Fatalf("auto-pilot line = %q, want pause reason", line)
	}
	// The disabled branch must win over the transient autoInProgress state,
	// which would otherwise render a misleading "evaluating" line.
	if strings.Contains(line, "evaluating") {
		t.Fatalf("auto-pilot line = %q, want disabled to override evaluating", line)
	}
}

func TestGroupedControlsTextShowsDisabledAutoState(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		autopilotPaused: true,
		width:           100,
		height:          20,
		jobs:            []*db.Job{{ID: 202, Status: db.StatusQueued, Description: "queued"}},
	}
	m.rebuildGroupedRows()

	line := m.groupedControlsText(true)
	if !strings.Contains(line, "(OFF)") {
		t.Fatalf("controls line = %q, want auto state OFF", line)
	}
	if strings.Contains(line, "(ON)") {
		t.Fatalf("controls line = %q, want OFF not ON", line)
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

func TestCollectLaunchIDsForListIncludesOpenMoveAttempts(t *testing.T) {
	sourceLaunchID := int64(3946)
	targetLaunchID := int64(3978)
	sourceAttemptID := int64(91)
	targetAttemptID := int64(92)
	job := &db.Job{ID: 3190, Status: db.StatusRunning, LaunchID: &sourceLaunchID}

	got := collectLaunchIDsForList([]*db.Job{job}, map[int64]jobview.PlacementStatus{
		3190: {
			JobID: 3190,
			Move: &jobview.MoveDisplay{
				State:           db.MoveIntentStateOpen,
				SourceAttemptID: &sourceAttemptID,
				TargetAttemptID: &targetAttemptID,
				AttemptsByID: map[int64]db.JobAttempt{
					sourceAttemptID: {ID: sourceAttemptID, JobID: 3190, LaunchID: &sourceLaunchID},
					targetAttemptID: {ID: targetAttemptID, JobID: 3190, LaunchID: &targetLaunchID},
				},
			},
		},
	})

	if len(got) != 2 {
		t.Fatalf("launch IDs = %v, want source and target", got)
	}
	if got[0] != sourceLaunchID || got[1] != targetLaunchID {
		t.Fatalf("launch IDs = %v, want [%d %d]", got, sourceLaunchID, targetLaunchID)
	}
}

func TestListTUIJobsLoadedClearsStalePersistentBlockedSummaryWhenNoUnplacedJobs(t *testing.T) {
	m := listTUIModel{
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

func TestListTUIReloadRequestsCoalesceWhileReloadInProgress(t *testing.T) {
	m := listTUIModel{
		reloadInProgress: true,
	}

	cmd := m.requestReloadJobs()
	if cmd != nil {
		t.Fatal("reload request during active reload should not start another command")
	}
	if !m.reloadPending {
		t.Fatal("reload request during active reload should mark a trailing reload")
	}

	next, trailingCmd := m.Update(listJobsLoadedMsg{})
	got := next.(listTUIModel)
	if trailingCmd == nil {
		t.Fatal("completed reload with pending request should schedule one trailing reload")
	}
	if !got.reloadInProgress {
		t.Fatal("trailing reload command should mark reloadInProgress")
	}
	if got.reloadPending {
		t.Fatal("reloadPending should clear when trailing reload is scheduled")
	}
}

func TestNewListTUIModelDelegatesBackgroundWorkToLiveDaemon(t *testing.T) {
	database := db.SetupTestDB(t)
	jobs := []*db.Job{{ID: 1, Status: db.StatusQueued, Tags: []string{"rental"}}}

	m := newListTUIModel(database, nil, jobs, "Jobs", true, true, "")
	defer m.shutdown()

	if m.syncEnabled {
		t.Fatal("syncEnabled should be disabled when the live daemon owns background sync")
	}
	if m.syncWorker != nil {
		t.Fatal("syncWorker should not start when the live daemon owns background sync")
	}
	if cmd := m.runAutoPilot(); cmd != nil {
		t.Fatal("runAutoPilot should not start a pass when the live daemon owns autopilot")
	}
	if m.autoInProgress {
		t.Fatal("runAutoPilot should leave autoInProgress false when delegated to the daemon")
	}
}

func TestNewListTUIModelKeepsForegroundBackgroundWorkWhenDaemonStopped(t *testing.T) {
	withStoppedDaemonStatus(t)
	database := db.SetupTestDB(t)

	m := newListTUIModel(database, nil, nil, "Jobs", true, true, "")
	defer m.shutdown()

	if !m.syncEnabled {
		t.Fatal("syncEnabled should remain true when no live daemon is available")
	}
	if m.syncWorker == nil {
		t.Fatal("syncWorker should start when no live daemon is available")
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
	m := listTUIModel{}
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
		width:           64,
		height:          12,
		title:           "Jobs",
		jobs: []*db.Job{
			{ID: 733, Status: db.StatusQueued, Description: "retry pending", Project: "proj"},
		},
		statusMessage: "Auto-pilot failed: no cloud providers available: vastai: vastai CLI availability check failed: timeout",
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "A:auto (ON)") || !strings.Contains(out, "q:quit") {
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

func TestListTUISyncFinishedDoesNotPersistCloudTimeoutWarning(t *testing.T) {
	m := listTUIModel{
		jobs: []*db.Job{
			{ID: 733, Status: db.StatusQueued, Description: "retry pending", Project: "proj"},
		},
		pendingSyncHosts: map[string]struct{}{backgroundSyncKey: {}},
	}

	next, _ := m.Update(listSyncFinishedMsg{
		full:     true,
		warnings: []string{degraded.CloudSyncTimedOutWaitingForDB("1m0s")},
	})
	got := next.(listTUIModel)
	if got.statusMessage != "" {
		t.Fatalf("statusMessage = %q, want cloud timeout warning cleared", got.statusMessage)
	}
}

func TestListTUISyncFinishedKeepsNonCloudTimeoutWarnings(t *testing.T) {
	m := listTUIModel{
		jobs: []*db.Job{
			{ID: 733, Status: db.StatusQueued, Description: "retry pending", Project: "proj"},
		},
		pendingSyncHosts: map[string]struct{}{backgroundSyncKey: {}},
	}

	next, _ := m.Update(listSyncFinishedMsg{
		full:     true,
		warnings: []string{degraded.CloudSyncTimedOutWaitingForDB("1m0s"), "R2 storage unreachable"},
	})
	got := next.(listTUIModel)
	if got.statusMessage != "R2 storage unreachable" {
		t.Fatalf("statusMessage = %q, want non-timeout warning", got.statusMessage)
	}
}

func TestListTUIGroupedViewShortViewportPreservesAllSectionHeaders(t *testing.T) {
	resetProviderCreditWarningCacheForTest(t)
	m := listTUIModel{
		groupedByStatus: true,
		width:           100,
		height:          10,
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

func TestListTUIInstanceHealthFooterToken(t *testing.T) {
	now := time.Now()
	m := listTUIModel{
		groupedByStatus: true,
		width:           140,
		height:          40,
		title:           "Jobs",
		recentFailedInstances: &recentFailedInstances{
			items: []*db.Launch{
				{
					ID:                 77,
					Status:             db.LaunchStatusFailed,
					Provider:           "vastai",
					ProviderInstanceID: "provider-77",
					TerminationReason:  db.TerminationReasonInfraFailure,
					TerminationDetail:  "provider reported container exited before bootstrap completed with code 137",
					EndedAt:            testInt64Ptr(now.Add(-2 * time.Minute).Unix()),
					ResolvedGPUName:    "RTX 4090",
					GPUMemGB:           24,
					CostPerHourCents:   120,
				},
			},
			jobOutcomeByLaunchID: map[int64]db.LaunchJobOutcome{
				77: {JobID: 1, Status: db.StatusFailed},
			},
		},
	}
	m.rebuildGroupedRows()

	// Recent abnormal terminations are summarized in the footer, not as
	// selectable job-status rows, and the inline section header is gone.
	if len(m.groupedSelectableRows) != 0 {
		t.Fatalf("selectable rows = %d, want 0 (failed instances are footer-only now)", len(m.groupedSelectableRows))
	}
	out := stripANSI(m.View())
	if strings.Contains(out, "Recent failed instances") {
		t.Fatalf("interactive view must not render the inline section, got:\n%s", out)
	}
	for _, want := range []string{"Launch failures: 1 failed", "f:diagnose"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %q in one-line footer, got:\n%s", want, out)
		}
	}

	// f opens the diagnose overlay with the full grouped breakdown.
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	m = next.(listTUIModel)
	if !m.showInstanceFailures {
		t.Fatal("f should open the instance-failures overlay")
	}
	overlay := stripANSI(m.View())
	for _, want := range []string{"Instance failures", "failed (1)", "esc/q back"} {
		if !strings.Contains(overlay, want) {
			t.Fatalf("overlay should render %q, got:\n%s", want, overlay)
		}
	}

	// esc returns to the list.
	back, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if back.(listTUIModel).showInstanceFailures {
		t.Fatal("esc should close the instance-failures overlay")
	}
}

func TestGroupedViewportCountsFailedInstanceRows(t *testing.T) {
	now := time.Unix(10_000, 0)
	failures := &recentFailedInstances{
		items: []*db.Launch{
			{ID: 81, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonUnknown, TerminationDetail: "provider dead", EndedAt: testInt64Ptr(9_900)},
			{ID: 82, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonJobFailure, TerminationDetail: "job failed", EndedAt: testInt64Ptr(9_901)},
			{ID: 83, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonInfraFailure, TerminationDetail: "infra failed", EndedAt: testInt64Ptr(9_902)},
		},
	}
	rows := buildGroupedStatusRowsWithOptions(nil, 100, groupedStatusRenderOptions{
		failedInstances: failures,
		now:             now,
	})

	lines := selectGroupedRowsForViewport(rows, 20, -1)
	if len(lines) == 0 {
		t.Fatal("selectGroupedRowsForViewport returned no lines")
	}
	got := stripANSI(lines[0].text)
	if !strings.HasPrefix(got, "Recent failed instances") || !strings.Contains(got, "(2):") {
		t.Fatalf("header = %q, want failed instance count of 2", got)
	}
	for _, line := range lines {
		if strings.Contains(stripANSI(line.text), "job failed") {
			t.Fatalf("normal job failure should be hidden from failed instances: %q", line.text)
		}
	}
	if strings.Contains(got, "(0)") {
		t.Fatalf("header counted jobs instead of failed instances: %q", got)
	}
}

func TestListTUIInstanceFailuresOverlayContent(t *testing.T) {
	now := time.Now()
	m := listTUIModel{
		groupedByStatus: true,
		width:           120,
		height:          24,
		title:           "Jobs",
		recentFailedInstances: &recentFailedInstances{
			items: []*db.Launch{
				{ID: 91, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonProviderFailure, TerminationDetail: "transient", EndedAt: testInt64Ptr(now.Add(-5 * time.Minute).Unix())},
				{ID: 92, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonProviderFailure, TerminationDetail: "transient", EndedAt: testInt64Ptr(now.Add(-6 * time.Minute).Unix())},
				{ID: 93, Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonInfraFailure, TerminationDetail: "no agent", EndedAt: testInt64Ptr(now.Add(-7 * time.Minute).Unix())},
			},
			// 91/92 succeeded, 93 is a dud.
			jobOutcomeByLaunchID: map[int64]db.LaunchJobOutcome{
				91: {JobID: 1, Status: db.StatusCompleted},
				92: {JobID: 2, Status: db.StatusCompleted},
			},
		},
	}
	m.rebuildGroupedRows()

	// The one-line footer leads with the attention bucket (the dud); the
	// recovered (succeeded) detail belongs in the overlay, not the footer.
	footer := stripANSI(m.View())
	if !strings.Contains(footer, "Launch failures: 1 dud") || !strings.Contains(footer, "f:diagnose") {
		t.Fatalf("footer should be a one-line dud token:\n%s", footer)
	}
	if strings.Contains(footer, "succeeded") {
		t.Fatalf("recovered detail should not be in the footer:\n%s", footer)
	}

	// f opens the overlay, which reveals the full grouped-by-outcome breakdown.
	opened, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	m = opened.(listTUIModel)
	overlay := stripANSI(m.View())
	for _, want := range []string{"succeeded (2)", "dud (1)"} {
		if !strings.Contains(overlay, want) {
			t.Fatalf("overlay should reveal %q:\n%s", want, overlay)
		}
	}

	// q also closes the overlay.
	closed, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if closed.(listTUIModel).showInstanceFailures {
		t.Fatal("q should close the overlay")
	}
}

func TestListTUIArrowKeysExpandPlacementBreakdown(t *testing.T) {
	job := &db.Job{
		ID:     42,
		Status: db.StatusQueued,
		PlacementBlockedJSON: (&blockreason.Structured{
			Summary: "no compatible hosts",
			Launch:  "credit exhausted",
			Reuse: []blockreason.ReuseRejection{
				{Instance: "wi1", Reason: "disk insufficient"},
			},
		}).Marshal(),
	}
	newModel := func(expanded bool) listTUIModel {
		m := listTUIModel{
			groupedByStatus: true,
			width:           120,
			height:          24,
			title:           "Jobs",
			jobs:            []*db.Job{job},
		}
		if expanded {
			m.expandedBlocked = map[int64]bool{job.ID: true}
		}
		m.rebuildGroupedRows()
		return m
	}

	rightNext, _ := newModel(false).Update(tea.KeyMsg{Type: tea.KeyRight})
	if !rightNext.(listTUIModel).expandedBlocked[job.ID] {
		t.Fatal("Right arrow should expand a collapsed placement breakdown")
	}
	leftNext, _ := newModel(true).Update(tea.KeyMsg{Type: tea.KeyLeft})
	if leftNext.(listTUIModel).expandedBlocked[job.ID] {
		t.Fatal("Left arrow should collapse an expanded placement breakdown")
	}
	enterNext, _ := newModel(false).Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !enterNext.(listTUIModel).expandedBlocked[job.ID] {
		t.Fatal("Enter should still toggle placement breakdown expansion")
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

	lines := selectGroupedRowsForViewport(rows, 4, -1)
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

	lines := selectGroupedRowsForViewport(rows, 1, -1)
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

	lines := selectGroupedRowsForViewport(rows, 3, -1)
	if len(lines) != 2 {
		t.Fatalf("line count = %d, want 2", len(lines))
	}
	if strings.TrimSpace(lines[0].text) == "" || strings.TrimSpace(lines[1].text) == "" {
		t.Fatalf("expected no blank separator between abbreviated groups, got: %+v", lines)
	}
}

// groupedRowIndexForJob returns the index of the row carrying the given job ID,
// i.e. a valid cursorRowIdx for selectGroupedRowsForViewport.
func groupedRowIndexForJob(rows []groupedStatusRow, jobID int64) int {
	for i := range rows {
		if rows[i].job != nil && rows[i].job.ID == jobID {
			return i
		}
	}
	return -1
}

func joinViewportText(lines []groupedViewportLine) string {
	parts := make([]string, len(lines))
	for i, l := range lines {
		parts[i] = stripANSI(l.text)
	}
	return strings.Join(parts, "\n")
}

// reportedGroupedJobs reproduces the bug-report layout: Running(4), Queued(1),
// Completed(5), Failed(10).
func reportedGroupedJobs() []*db.Job {
	jobs := make([]*db.Job, 0, 20)
	for i := 1; i <= 4; i++ {
		jobs = append(jobs, &db.Job{ID: int64(i), Status: db.StatusRunning, Host: "cool30", Description: fmt.Sprintf("run %d", i), Project: "proj"})
	}
	jobs = append(jobs, &db.Job{ID: 50, Status: db.StatusQueued, Host: "cool30", Description: "queued 1", Project: "proj"})
	for i := 0; i < 5; i++ {
		jobs = append(jobs, &db.Job{ID: int64(100 + i), Status: db.StatusCompleted, ExitCode: testIntPtr(0), Description: fmt.Sprintf("done %d", i), Project: "proj"})
	}
	for i := 0; i < 10; i++ {
		jobs = append(jobs, &db.Job{ID: int64(200 + i), Status: db.StatusFailed, Description: fmt.Sprintf("fail %d", i), Project: "proj"})
	}
	return jobs
}

func manyTerminalGroupedJobs(completed, failed int) []*db.Job {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: "cool30", Description: "run 1", Project: "proj"},
		{ID: 2, Status: db.StatusRunning, Host: "cool30", Description: "run 2", Project: "proj"},
		{ID: 3, Status: db.StatusQueued, Host: "cool30", Description: "queued 1", Project: "proj"},
	}
	for i := 0; i < completed; i++ {
		jobs = append(jobs, &db.Job{ID: int64(1000 + i), Status: db.StatusCompleted, ExitCode: testIntPtr(0), Description: fmt.Sprintf("done %d", i), Project: "proj"})
	}
	for i := 0; i < failed; i++ {
		jobs = append(jobs, &db.Job{ID: int64(2000 + i), Status: db.StatusFailed, Description: fmt.Sprintf("fail %d", i), Project: "proj"})
	}
	return jobs
}

func BenchmarkSelectGroupedRowsForViewportManyTerminalRows(b *testing.B) {
	rows := buildGroupedStatusRows(manyTerminalGroupedJobs(100, 100), 120, nil, nil)
	cursorRowIdx := groupedRowIndexForJob(rows, 2050)
	if cursorRowIdx < 0 {
		b.Fatal("cursor job row not found")
	}
	b.ReportAllocs()
	for b.Loop() {
		lines := selectGroupedRowsForViewport(rows, 20, cursorRowIdx)
		if len(lines) == 0 {
			b.Fatal("empty viewport")
		}
	}
}

func BenchmarkListTUIGroupedViewManyTerminalRows(b *testing.B) {
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		width:           120,
		height:          24,
		jobs:            manyTerminalGroupedJobs(100, 100),
	}
	m.rebuildGroupedRows()
	for pos, rowIdx := range m.groupedSelectableRows {
		if j := m.groupedRows[rowIdx].job; j != nil && j.ID == 2050 {
			m.cursor = pos
			break
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		out := m.View()
		if !strings.Contains(out, "wj2050") {
			b.Fatal("cursor job not visible")
		}
	}
}

// TestSelectGroupedRowsForViewport_CursorRowAlwaysVisible is the regression test
// for the reported bug: arrow navigation onto a job in an abbreviated section
// must keep that job on screen. The cursor sits on the last Completed job, which
// the cursor-agnostic abbreviation would have elided behind "...".
func TestSelectGroupedRowsForViewport_CursorRowAlwaysVisible(t *testing.T) {
	rows := buildGroupedStatusRows(reportedGroupedJobs(), 120, nil, nil)

	for _, tc := range []struct {
		name  string
		jobID int64
	}{
		{"last completed (abbreviated tail)", 104},
		{"middle failed (legacy collapses first)", 205},
		{"last failed", 209},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cursorRowIdx := groupedRowIndexForJob(rows, tc.jobID)
			if cursorRowIdx < 0 {
				t.Fatalf("job %d row not found", tc.jobID)
			}
			lines := selectGroupedRowsForViewport(rows, 12, cursorRowIdx)
			if !viewportContains(lines, cursorRowIdx) {
				t.Fatalf("cursor row for job %d not visible:\n%s", tc.jobID, joinViewportText(lines))
			}
			if len(lines) > 12 {
				t.Fatalf("viewport exceeded budget: %d > 12", len(lines))
			}
		})
	}
}

// TestSelectGroupedRowsForViewport_AnchorsWindowWithinSection verifies the cursor
// section shows a window centered on the cursor with a leading "..." when the
// window starts past the section head.
func TestSelectGroupedRowsForViewport_AnchorsWindowWithinSection(t *testing.T) {
	jobs := make([]*db.Job, 0, 10)
	for i := 0; i < 10; i++ {
		jobs = append(jobs, &db.Job{ID: int64(300 + i), Status: db.StatusCompleted, ExitCode: testIntPtr(0), Description: fmt.Sprintf("done %d", i), Project: "proj"})
	}
	rows := buildGroupedStatusRows(jobs, 120, nil, nil)
	cursorRowIdx := groupedRowIndexForJob(rows, 307) // a middle-tail job
	if cursorRowIdx < 0 {
		t.Fatal("cursor job row not found")
	}

	lines := selectGroupedRowsForViewport(rows, 6, cursorRowIdx)
	if !viewportContains(lines, cursorRowIdx) {
		t.Fatalf("cursor row not visible:\n%s", joinViewportText(lines))
	}
	cursorLine, firstEllipsis := -1, -1
	for i, l := range lines {
		if l.rowIdx == cursorRowIdx {
			cursorLine = i
		}
		if firstEllipsis < 0 && strings.TrimSpace(l.text) == "..." {
			firstEllipsis = i
		}
	}
	if firstEllipsis < 0 || firstEllipsis >= cursorLine {
		t.Fatalf("expected a leading ellipsis before the cursor row, got:\n%s", joinViewportText(lines))
	}
}

// TestListTUIGroupedViewRevealsCursorInAbbreviatedSection drives the full
// View() pipeline: with the cursor on a job that a height-limited viewport would
// abbreviate, the rendered output must still contain that job.
func TestListTUIGroupedViewRevealsCursorInAbbreviatedSection(t *testing.T) {
	resetProviderCreditWarningCacheForTest(t)
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		width:           120,
		height:          16,
		jobs:            reportedGroupedJobs(),
	}
	m.rebuildGroupedRows()

	// Place the cursor on the last Completed job (wj104), which sits behind the
	// "..." tail of an abbreviated section.
	target := -1
	for pos, rowIdx := range m.groupedSelectableRows {
		if j := m.groupedRows[rowIdx].job; j != nil && j.ID == 104 {
			target = pos
			break
		}
	}
	if target < 0 {
		t.Fatal("completed job wj104 not selectable")
	}
	m.cursor = target

	out := stripANSI(m.View())
	if !strings.Contains(out, "wj104") {
		t.Fatalf("cursor job wj104 not visible in rendered view:\n%s", out)
	}
}

func TestSelectGroupedRowsForViewport_DoesNotSpendEllipsisOnSingleHiddenRow(t *testing.T) {
	launchID := int64(3816)
	jobs := []*db.Job{
		{ID: 2982, Status: db.StatusRunning, Host: db.LaunchHost(launchID), LaunchID: &launchID, Project: "structural-probes", Description: "EXP-032/RQ33: GPT-2-medium", StartTime: 1},
		{ID: 2994, Status: db.StatusRunning, Host: "cool30", Project: "markov-attention", Description: "EXP-218 Eleuther", StartTime: 1},
		{ID: 3001, Status: db.StatusRunning, Host: db.LaunchHost(launchID), LaunchID: &launchID, Project: "markov-attention", Description: "EXP-218 gpt2-large", StartTime: 1},
		{ID: 2991, Status: db.StatusQueued, Host: db.LaunchHost(launchID), LaunchID: &launchID, Project: "markov-attention", Description: "EXP-218 EleutherAI/pythia-160m", QueuedAt: 1},
		{ID: 2992, Status: db.StatusQueued, Host: db.LaunchHost(launchID), LaunchID: &launchID, Project: "markov-attention", Description: "EXP-218 gpt2-medium", QueuedAt: 1},
		{ID: 3000, Status: db.StatusQueued, Host: db.LaunchHost(launchID), LaunchID: &launchID, Project: "llm-performance-models", Description: "EXP-344 Tier1 PILOT", QueuedAt: 1},
		{ID: 2998, Status: db.StatusPendingPlacement, Host: db.LaunchHost(launchID), LaunchID: &launchID, Project: "continuous-thought", Description: "EXP-059 lr-sweep", CreatedAt: 1},
		{ID: 2997, Status: db.StatusPendingPlacement, Project: "continuous-thought", Description: "EXP-059 lr-sweep lr=2e-5", CreatedAt: 1, PlacementReasons: []string{"could not reuse running instances: wi3816 RTX A6000 45GB: GPU memory insufficient: job=48GB"}},
		{ID: 2999, Status: db.StatusPendingPlacement, Project: "continuous-thought", Description: "EXP-059 lr-sweep lr=5e-5", CreatedAt: 1, PlacementReasons: []string{"could not reuse running instances: wi3816 RTX A6000 45GB: GPU memory insufficient: job=48GB"}},
		{ID: 2966, Status: db.StatusCompleted, ExitCode: testIntPtr(0), Project: "chunk-boundary-probes", Description: "EXP-003: BLT pretrained boundary", EndTime: testInt64Ptr(2)},
		{ID: 2995, Status: db.StatusCompleted, ExitCode: testIntPtr(0), Project: "markov-attention", Description: "EXP-218 EleutherAI/pythia-1.4b", EndTime: testInt64Ptr(2)},
		{ID: 2949, Status: db.StatusFailed, Project: "structural-probes", Description: "EXP-032/RQ33: GPT-2-medium direct distance", EndTime: testInt64Ptr(2)},
		{ID: 2950, Status: db.StatusFailed, Project: "structural-probes", Description: "EXP-116/RQ68: Gamma aleatoric", EndTime: testInt64Ptr(2)},
		{ID: 2964, Status: db.StatusFailed, Project: "chunk-boundary-probes", Description: "EXP-003: BLT pretrained boundary export", EndTime: testInt64Ptr(2)},
		{ID: 2978, Status: db.StatusFailed, Project: "continuous-thought", Description: "EXP-059 perturbation", EndTime: testInt64Ptr(2)},
		{ID: 2954, Status: db.StatusKilled, Project: "chunk-boundary-probes", Description: "EXP-003: BLT pretrained boundary export", EndTime: testInt64Ptr(2)},
	}
	rows := buildGroupedStatusRows(jobs, 120, nil, nil)
	cursorRowIdx := groupedRowIndexForJob(rows, 2966)
	if cursorRowIdx < 0 {
		t.Fatal("completed job wj2966 row not found")
	}

	lines := selectGroupedRowsForViewport(rows, 29, cursorRowIdx)
	text := joinViewportText(lines)
	if !strings.Contains(text, "wj3001") {
		t.Fatalf("third running job should replace a same-cost ellipsis:\n%s", text)
	}
	if strings.Contains(text, "Running (3):\n-   wj2982") && strings.Contains(text, "\n...\n\nQueued (3):") {
		t.Fatalf("running section used ellipsis despite room for the hidden row:\n%s", text)
	}
	if len(lines) > 29 {
		t.Fatalf("viewport exceeded budget: %d > 29\n%s", len(lines), text)
	}
}

func TestSelectGroupedRowsForViewport_ReopensSmallCollapsedRunningSection(t *testing.T) {
	launchID := int64(4092)
	jobs := []*db.Job{
		{ID: 3382, Status: db.StatusRunning, Host: db.LaunchHost(launchID), LaunchID: &launchID, Project: "progressive-weight-streaming", Description: "running 1", StartTime: 1},
		{ID: 3384, Status: db.StatusRunning, Host: db.LaunchHost(launchID), LaunchID: &launchID, Project: "progressive-weight-streaming", Description: "running 2", StartTime: 1},
		{ID: 3390, Status: db.StatusQueued, Host: db.LaunchHost(launchID), LaunchID: &launchID, Project: "llm-performance-models", Description: "agent starting", QueuedAt: 1},
		{ID: 3385, Status: db.StatusQueued, Project: "llm-performance-models", Description: "MoE probe", CreatedAt: 1},
		{ID: 3389, Status: db.StatusQueued, Project: "progressive-weight-streaming", Description: "retry pending", CreatedAt: 1},
	}
	for i := 0; i < 12; i++ {
		jobs = append(jobs, &db.Job{ID: int64(3400 + i), Status: db.StatusCompleted, ExitCode: testIntPtr(0), Project: "llm-performance-models", Description: fmt.Sprintf("completed %d", i), EndTime: testInt64Ptr(2)})
	}
	for i := 0; i < 13; i++ {
		jobs = append(jobs, &db.Job{ID: int64(3500 + i), Status: db.StatusFailed, Project: "llm-performance-models", Description: fmt.Sprintf("failed %d", i), EndTime: testInt64Ptr(2)})
	}

	rows := buildGroupedStatusRowsWithOptions(jobs, 120, groupedStatusRenderOptions{
		launchStatusByID: map[int64]string{launchID: db.LaunchStatusLaunching},
		now:              time.Unix(10_000, 0),
	})
	cursorRowIdx := groupedRowIndexForJob(rows, 3385)
	if cursorRowIdx < 0 {
		t.Fatal("unplaced job wj3385 row not found")
	}

	lines := selectGroupedRowsForViewport(rows, 24, cursorRowIdx)
	text := joinViewportText(lines)
	for _, want := range []string{"Running (2):", "wj3382", "wj3384", "Unplaced (2):", "wj3385"} {
		if !strings.Contains(text, want) {
			t.Fatalf("viewport should keep small running section visible; missing %q:\n%s", want, text)
		}
	}
	if len(lines) > 24 {
		t.Fatalf("viewport exceeded budget: %d > 24\n%s", len(lines), text)
	}
}

func TestSelectGroupedRowsForViewport_PreservesSelectedSectionBeforeOtherSections(t *testing.T) {
	now := time.Unix(10_000, 0)
	blockedReason := "prepare R2 assets: upload agent to R2: upload agent binary: put object agents/linux-amd64: operation error S3: PutObject, context deadline exceeded"
	jobs := []*db.Job{
		{ID: 3392, Status: db.StatusQueued, Project: "progressive-weight-streaming", Description: "EXP-003 make-or-break", CreatedAt: 1, QueueBlockedReason: blockedReason},
		{ID: 3393, Status: db.StatusQueued, Project: "progressive-weight-streaming", Description: "EXP-007b cue sweep cross-filter", CreatedAt: 1, QueueBlockedReason: blockedReason},
		{ID: 3394, Status: db.StatusQueued, Project: "progressive-weight-streaming", Description: "EXP-007b cue sweep large-only", CreatedAt: 1, QueueBlockedReason: blockedReason},
		{ID: 3395, Status: db.StatusQueued, Project: "llm-performance-models", Description: "prefill backfill: allenai/OLMoE", CreatedAt: 1, PlacementReasons: []string{"placement pending"}},
		{ID: 3396, Status: db.StatusQueued, Project: "llm-performance-models", Description: "prefill backfill: Qwen", CreatedAt: 1, PlacementReasons: []string{"placement pending"}},
		{ID: 3397, Status: db.StatusQueued, Project: "llm-performance-models", Description: "prefill backfill: deepseek", CreatedAt: 1, PlacementReasons: []string{"placement pending"}},
		{ID: 3398, Status: db.StatusQueued, Project: "llm-performance-models", Description: "prefill backfill: granite", CreatedAt: 1, PlacementReasons: []string{"placement pending"}},
		{ID: 3399, Status: db.StatusQueued, Project: "llm-performance-models", Description: "big sibling", CreatedAt: 1, PlacementReasons: []string{"placement pending"}},
		{ID: 3385, Status: db.StatusQueued, Project: "llm-performance-models", Description: "MoE probe", CreatedAt: 1, EndTime: testInt64Ptr(9_900)},
	}
	for i := 0; i < 12; i++ {
		jobs = append(jobs, &db.Job{ID: int64(3346 + i), Status: db.StatusCompleted, ExitCode: testIntPtr(0), Project: "llm-performance-models", Description: fmt.Sprintf("completed %d", i), EndTime: testInt64Ptr(2)})
	}
	for i := 0; i < 12; i++ {
		jobs = append(jobs, &db.Job{ID: int64(3308 + i), Status: db.StatusFailed, Project: "dependency-routing", Description: fmt.Sprintf("failed %d", i), EndTime: testInt64Ptr(2)})
	}

	rows := buildGroupedStatusRowsWithOptions(jobs, 120, groupedStatusRenderOptions{now: now})
	cursorRowIdx := groupedRowIndexForJob(rows, 3395)
	if cursorRowIdx < 0 {
		t.Fatal("unplaced job wj3395 row not found")
	}

	lines := selectGroupedRowsForViewport(rows, 24, cursorRowIdx)
	text := joinViewportText(lines)
	for _, want := range []string{
		"Unplaced (9):",
		"blocked: prepare R2 assets",
		"waiting: autopilot",
		"wj3395",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("selected Unplaced section should keep local context; missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Unplaced (9):\n...") {
		t.Fatalf("selected section was abbreviated before other sections:\n%s", text)
	}
	if len(lines) > 24 {
		t.Fatalf("viewport exceeded budget: %d > 24\n%s", len(lines), text)
	}
}

func TestListTUIGroupedViewPinsFooterAtBottom(t *testing.T) {
	resetProviderCreditWarningCacheForTest(t)
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
	// The auto-pilot status line sits directly above the controls footer.
	if !strings.Contains(lines[len(lines)-2], "Auto-pilot:") {
		t.Fatalf("expected auto-pilot status line above footer, got %q", lines[len(lines)-2])
	}
}

func TestListTUIGroupedViewPlacesSharedStatusAboveControls(t *testing.T) {
	database := db.SetupTestDB(t)
	m := listTUIModel{
		database:        database,
		groupedByStatus: true,
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
	withStoppedDaemonStatus(t)
	m := listTUIModel{
		groupedByStatus: true,
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

func TestListTUIBackgroundCloudSyncDedupesWhileInFlight(t *testing.T) {
	m := listTUIModel{
		pendingSyncHosts: make(map[string]struct{}),
		database:         db.SetupTestDB(t),
	}

	cmds := []tea.Cmd{}
	cmds = m.enqueueBackgroundCloudSync(cmds, true)
	if len(cmds) != 1 {
		t.Fatalf("commands after first enqueue = %d, want 1", len(cmds))
	}
	if _, ok := m.pendingSyncHosts[backgroundSyncKey]; !ok {
		t.Fatal("background cloud sync should be marked in-flight")
	}

	cmds = m.enqueueBackgroundCloudSync(cmds, true)
	if len(cmds) != 1 {
		t.Fatalf("commands after duplicate enqueue = %d, want 1", len(cmds))
	}

	delete(m.pendingSyncHosts, backgroundSyncKey)
	cmds = m.enqueueBackgroundCloudSync(cmds, true)
	if len(cmds) != 2 {
		t.Fatalf("commands after completed sync = %d, want 2", len(cmds))
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
			database:        &sql.DB{},
			autoNextPassAt:  time.Now().Add(30 * time.Second),
		}
	}

	t.Run("toggle auto on", func(t *testing.T) {
		database := db.SetupTestDB(t)
		if _, err := db.PauseAutopilot(database, "tester", ""); err != nil {
			t.Fatalf("PauseAutopilot: %v", err)
		}
		m := listTUIModel{
			groupedByStatus: true,
			database:        database,
			autopilotPaused: true,
			autoNextPassAt:  time.Now().Add(30 * time.Second),
		}
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'A'}})
		got := next.(listTUIModel)
		if got.autopilotPaused {
			t.Fatal("expected autopilot enabled after 'A' toggle")
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
		m.database = setupBudgetTest(t)
		m.autoRunRateInputActive = true
		m.autoRunRateInputPhase = autoBudgetPhaseEditHourly
		m.autoRunRateInputValue = "3.50"
		next, _ := m.handleAutoRunRateInputKey(tea.KeyMsg{Type: tea.KeyEnter})
		got := next.(listTUIModel)
		if got.autoRunRateInputPhase != autoBudgetPhaseMenu {
			t.Fatalf("after save expected menu phase, got %v", got.autoRunRateInputPhase)
		}
		if got.autoRunRateInputActive {
			t.Fatal("save should close the panel")
		}
		if !got.autoNextPassAt.IsZero() {
			t.Fatalf("expected cooldown cleared after save, got %v", got.autoNextPassAt)
		}
	})
}

func TestListTUIRunawayResetClearsCachedBlockedRows(t *testing.T) {
	database := db.SetupTestDB(t)
	jobs := []*db.Job{
		{ID: 10, Status: db.StatusQueued, Description: "retry job"},
	}
	m := listTUIModel{
		groupedByStatus:        true,
		groupedUnprocessedView: true,
		database:               database,
		jobs:                   jobs,
		width:                  120,
		height:                 40,
		autoBlockReasons: map[int64]string{
			10: "paused: repeated failed instances without progress",
		},
		autoPersistentBlocked:  "paused: repeated failed instances without progress",
		autoPersistentBlockedN: 1,
		autoRunRateInputActive: true,
		autoRunRateInputPhase:  autoBudgetPhaseMenu,
	}
	m.rebuildGroupedRows()

	next, _ := m.handleAutoRunRateInputKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	got := next.(listTUIModel)

	if len(got.autoBlockReasons) != 0 {
		t.Fatalf("autoBlockReasons = %v, want cleared", got.autoBlockReasons)
	}
	if got.autoPersistentBlocked != "" || got.autoPersistentBlockedN != 0 {
		t.Fatalf("persistent blocked = %q/%d, want cleared", got.autoPersistentBlocked, got.autoPersistentBlockedN)
	}
	infos, err := campaign.LookupActiveRunawayBreakers(database)
	if err != nil {
		t.Fatalf("runaway infos: %v", err)
	}
	if len(infos) != 0 {
		t.Fatalf("active runaway breaker infos = %v, want none", infos)
	}
	for _, row := range got.groupedRows {
		if strings.Contains(row.text, "paused: repeated failed instances") {
			t.Fatalf("stale blocked reason survived in row %q", row.text)
		}
	}
}

func TestListTUIAutoPilotFailureRebuildsGroupedRowsWithBlockReasons(t *testing.T) {
	jobs := []*db.Job{
		{ID: 10, Description: "test job", Tags: []string{"rental"}, Status: db.StatusQueued},
	}
	m := listTUIModel{
		groupedByStatus: true,
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
		width:           100,
		height:          20,
		jobs: []*db.Job{
			{ID: 202, Status: db.StatusQueued, Description: "queued"},
		},
	}
	m.rebuildGroupedRows()

	line := m.groupedControlsText(true)
	if !strings.Contains(line, "x:kill/cancel") || !strings.Contains(line, "u:unplace") || !strings.Contains(line, "p:toggle processed") || !strings.Contains(line, "m:move") || !strings.Contains(line, "N:new for selected") {
		t.Fatalf("controls line missing queued-job actions: %q", line)
	}
}

func TestListTUIGroupedKeyNLaunchesSelectedQueuedJob(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		width:           100,
		height:          20,
		jobs: []*db.Job{
			{ID: 301, Status: db.StatusQueued, Description: "queued"},
		},
	}
	m.rebuildGroupedRows()

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'N'}})
	got := next.(listTUIModel)
	if cmd == nil {
		t.Fatal("expected launch command for selected queued job")
	}
	if got.statusMessage != "Launching new instance for job #301..." {
		t.Fatalf("statusMessage = %q, want selected-job launch text", got.statusMessage)
	}
}

func TestListTUIGroupedKeyNRequiresSelectedQueuedJob(t *testing.T) {
	t.Run("no selection", func(t *testing.T) {
		m := listTUIModel{groupedByStatus: true}
		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'N'}})
		got := next.(listTUIModel)
		if cmd != nil {
			t.Fatal("expected no command without selected job")
		}
		if got.statusMessage != "Select a queued job row to launch" {
			t.Fatalf("statusMessage = %q, want selection error", got.statusMessage)
		}
	})

	t.Run("non queued", func(t *testing.T) {
		m := listTUIModel{
			groupedByStatus: true,
			width:           100,
			height:          20,
			jobs: []*db.Job{
				{ID: 302, Host: "studio", Status: db.StatusRunning, Description: "running"},
			},
		}
		m.rebuildGroupedRows()

		next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'N'}})
		got := next.(listTUIModel)
		if cmd != nil {
			t.Fatal("expected no command for non-queued job")
		}
		if got.statusMessage != "Can only launch queued jobs" {
			t.Fatalf("statusMessage = %q, want queued-only error", got.statusMessage)
		}
	})
}

func TestListTUIHandleLaunchNewDoneMessages(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		m := listTUIModel{}
		next, cmd := m.Update(moveExecuteDoneMsg{
			jobID:      303,
			action:     moveExecuteActionLaunchNew,
			targetDesc: "wi77",
		})
		got := next.(listTUIModel)
		if cmd == nil {
			t.Fatal("expected reload command after successful selected-job launch")
		}
		if got.statusMessage != "Launched new instance wi77 for job #303" {
			t.Fatalf("statusMessage = %q, want launch success", got.statusMessage)
		}
	})

	t.Run("failure", func(t *testing.T) {
		m := listTUIModel{}
		next, cmd := m.Update(moveExecuteDoneMsg{
			jobID:  304,
			action: moveExecuteActionLaunchNew,
			err:    errors.New("no compatible new-instance offer found"),
		})
		got := next.(listTUIModel)
		if cmd != nil {
			t.Fatal("expected no reload command after failed selected-job launch")
		}
		if got.statusMessage != "Launch failed: no compatible new-instance offer found" {
			t.Fatalf("statusMessage = %q, want launch failure", got.statusMessage)
		}
	})
}

func TestListTUIKeyV_CyclesGroupMode(t *testing.T) {
	m := listTUIModel{
		groupMode: listGroupUngrouped,
		width:     100,
		height:    20,
		jobs: []*db.Job{
			{ID: 1, Status: db.StatusQueued, Description: "queued"},
		},
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	got := next.(listTUIModel)
	if got.effectiveGroupMode() != listGroupStatus {
		t.Fatalf("group mode = %q, want status", got.effectiveGroupMode())
	}
	if got.statusMessage != "Grouped by status" {
		t.Fatalf("statusMessage = %q, want %q", got.statusMessage, "Grouped by status")
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	got = next.(listTUIModel)
	if got.effectiveGroupMode() != listGroupProject {
		t.Fatalf("group mode = %q, want project", got.effectiveGroupMode())
	}
	if got.statusMessage != "Grouped by project" {
		t.Fatalf("statusMessage = %q, want %q", got.statusMessage, "Grouped by project")
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	got = next.(listTUIModel)
	if got.effectiveGroupMode() != listGroupHost {
		t.Fatalf("group mode = %q, want host/instance", got.effectiveGroupMode())
	}

	next, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'v'}})
	got = next.(listTUIModel)
	if got.effectiveGroupMode() != listGroupUngrouped {
		t.Fatalf("group mode = %q, want ungrouped", got.effectiveGroupMode())
	}
	if got.statusMessage != "Ungrouped list view" {
		t.Fatalf("statusMessage = %q, want %q", got.statusMessage, "Ungrouped list view")
	}
}

func TestListTUIMouseClickSelectsFlatJobRow(t *testing.T) {
	jobs := []*db.Job{
		{ID: 101, Status: db.StatusRunning, Description: "first"},
		{ID: 102, Status: db.StatusQueued, Description: "second"},
		{ID: 103, Status: db.StatusQueued, Description: "third"},
	}
	m := listTUIModel{
		title:  "Jobs",
		jobs:   jobs,
		layout: newJobListLayout(100, jobs, nil, false),
		cursor: 0,
		width:  100,
		height: 12,
	}

	next, _ := m.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		Y:      3,
	})
	got := next.(listTUIModel)
	if got.cursor != 1 {
		t.Fatalf("cursor = %d, want 1", got.cursor)
	}
	if job := got.currentSelectedJob(); job == nil || job.ID != 102 {
		t.Fatalf("selected job = %+v, want wj102", job)
	}
}

func TestListTUIMouseClickIgnoresFlatFooter(t *testing.T) {
	jobs := []*db.Job{
		{ID: 101, Status: db.StatusRunning, Description: "first"},
		{ID: 102, Status: db.StatusQueued, Description: "second"},
	}
	m := listTUIModel{
		title:  "Jobs",
		jobs:   jobs,
		layout: newJobListLayout(100, jobs, nil, false),
		cursor: 0,
		width:  100,
		height: 8,
	}

	next, _ := m.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		Y:      7,
	})
	got := next.(listTUIModel)
	if got.cursor != 0 {
		t.Fatalf("cursor = %d, want unchanged 0", got.cursor)
	}
}

func TestListTUIMouseClickSelectsGroupedJobRow(t *testing.T) {
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		width:           100,
		height:          20,
		jobs: []*db.Job{
			{ID: 101, Status: db.StatusRunning, Description: "running"},
			{ID: 102, Status: db.StatusQueued, Description: "queued"},
		},
	}
	m.rebuildGroupedRows()
	clickY := groupedClickYForJob(t, m, 102)

	next, _ := m.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		Y:      clickY,
	})
	got := next.(listTUIModel)
	if job := got.selectedGroupedJob(); job == nil || job.ID != 102 {
		t.Fatalf("selected grouped job = %+v, want wj102", job)
	}
}

func TestListTUIMouseClickIgnoresGroupedHeaderAndFooter(t *testing.T) {
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		width:           100,
		height:          20,
		jobs: []*db.Job{
			{ID: 101, Status: db.StatusRunning, Description: "running"},
			{ID: 102, Status: db.StatusQueued, Description: "queued"},
		},
	}
	m.rebuildGroupedRows()
	m.cursor = 1

	next, _ := m.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		Y:      1,
	})
	got := next.(listTUIModel)
	if got.cursor != 1 {
		t.Fatalf("header click cursor = %d, want unchanged 1", got.cursor)
	}

	next, _ = got.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		Y:      19,
	})
	got = next.(listTUIModel)
	if got.cursor != 1 {
		t.Fatalf("footer click cursor = %d, want unchanged 1", got.cursor)
	}
}

func TestListTUIGroupedDaemonWarningShowsClickHint(t *testing.T) {
	withStoppedDaemonStatus(t)
	m := listTUIModel{
		title:                  "Jobs",
		groupedByStatus:        true,
		groupedUnprocessedView: true,
		width:                  100,
		height:                 12,
		jobs:                   nil,
	}
	m.rebuildGroupedRows()

	out := stripANSI(m.View())
	if !strings.Contains(out, "Daemon: stopped; click to start") {
		t.Fatalf("expected daemon click hint, got:\n%s", out)
	}
}

func TestListTUIGroupedDaemonWarningClickHintOnlyInUJ(t *testing.T) {
	withStoppedDaemonStatus(t)
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		width:           100,
		height:          12,
		jobs:            nil,
	}
	m.rebuildGroupedRows()

	out := stripANSI(m.View())
	if strings.Contains(out, "click to") {
		t.Fatalf("non-uj grouped view should not show daemon click hint, got:\n%s", out)
	}
}

func TestListTUIMouseClickGroupedDaemonWarningRestartsDaemon(t *testing.T) {
	withStoppedDaemonStatus(t)
	calls := 0
	oldRestart := listRestartDaemonFunc
	listRestartDaemonFunc = func() (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
		calls++
		return daemoncontrol.Status{PID: 4242, Live: true}, daemoncontrol.EnsureRestarted, nil
	}
	t.Cleanup(func() { listRestartDaemonFunc = oldRestart })

	m := listTUIModel{
		title:                  "Jobs",
		groupedByStatus:        true,
		groupedUnprocessedView: true,
		width:                  100,
		height:                 12,
		jobs:                   nil,
	}
	m.rebuildGroupedRows()
	layout := m.buildGroupedViewLayout(m.groupedRows, m.groupedJobsWithAutoReasons())
	if layout.daemonStatusY < 0 {
		t.Fatal("expected actionable daemon status row")
	}

	next, cmd := m.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		Y:      layout.daemonStatusY,
	})
	got := next.(listTUIModel)
	if got.statusMessage != "Restarting daemon..." {
		t.Fatalf("statusMessage = %q, want restart progress", got.statusMessage)
	}
	if cmd == nil {
		t.Fatal("expected restart command")
	}
	msg, ok := cmd().(listDaemonRestartedMsg)
	if !ok {
		t.Fatalf("restart command returned %T", msg)
	}
	if calls != 1 {
		t.Fatalf("restart calls = %d, want 1", calls)
	}

	next, _ = got.Update(msg)
	got = next.(listTUIModel)
	if got.statusMessage != "Daemon restarted (PID 4242)" {
		t.Fatalf("statusMessage = %q, want success", got.statusMessage)
	}
}

func TestListTUIDaemonRestartFailureMessage(t *testing.T) {
	m := listTUIModel{}
	next, _ := m.Update(listDaemonRestartedMsg{err: errors.New("boom")})
	got := next.(listTUIModel)
	if got.statusMessage != "Daemon restart failed: boom" {
		t.Fatalf("statusMessage = %q, want failure", got.statusMessage)
	}
}

func TestListTUISyncTickRestartsStaleDaemon(t *testing.T) {
	withStaleDaemonStatus(t)
	calls := 0
	oldRestart := listRestartDaemonFunc
	listRestartDaemonFunc = func() (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
		calls++
		return daemoncontrol.Status{PID: 5150, Live: true}, daemoncontrol.EnsureRestarted, nil
	}
	t.Cleanup(func() { listRestartDaemonFunc = oldRestart })

	m := listTUIModel{
		groupedByStatus:  true,
		pendingSyncHosts: map[string]struct{}{},
	}
	cmd := m.ensureCurrentDaemonForTick()
	if !m.daemonRestartInProgress {
		t.Fatal("expected daemon restart to be marked in progress")
	}
	if m.statusMessage != "Restarting daemon..." {
		t.Fatalf("statusMessage = %q, want restart progress", m.statusMessage)
	}
	if cmd == nil {
		t.Fatal("expected stale daemon check to return restart command")
	}
	msg, ok := cmd().(listDaemonRestartedMsg)
	if !ok {
		t.Fatalf("restart command returned %T", msg)
	}
	if msg.pid != 5150 {
		t.Fatalf("restart pid = %d, want 5150", msg.pid)
	}
	if calls != 1 {
		t.Fatalf("restart calls = %d, want 1", calls)
	}

	next, _ := m.Update(listDaemonRestartedMsg{pid: 5150, action: daemoncontrol.EnsureRestarted})
	got := next.(listTUIModel)
	if got.daemonRestartInProgress {
		t.Fatal("daemon restart in-progress flag was not cleared")
	}
	if got.statusMessage != "Daemon restarted (PID 5150)" {
		t.Fatalf("statusMessage = %q, want success", got.statusMessage)
	}
}

func TestListTUISyncTickDoesNotDuplicateDaemonRestart(t *testing.T) {
	withStaleDaemonStatus(t)
	oldRestart := listRestartDaemonFunc
	listRestartDaemonFunc = func() (daemoncontrol.Status, daemoncontrol.EnsureAction, error) {
		t.Fatal("restart function should not be called while restart is in progress")
		return daemoncontrol.Status{}, daemoncontrol.EnsureNoop, nil
	}
	t.Cleanup(func() { listRestartDaemonFunc = oldRestart })

	m := listTUIModel{
		groupedByStatus:         true,
		daemonRestartInProgress: true,
		pendingSyncHosts:        map[string]struct{}{},
	}
	cmd := m.ensureCurrentDaemonForTick()
	if cmd != nil {
		t.Fatal("expected no daemon restart command while restart is in progress")
	}
}

func TestListTUIMouseClickGroupedVastCreditWarningOpensBilling(t *testing.T) {
	SeedProviderCreditWarningForTesting(t, "WARNING: Vast.ai credits low ($5.00 < $10.00)")
	var opened []string
	oldOpen := listOpenURLFunc
	listOpenURLFunc = func(url string) error {
		opened = append(opened, url)
		return nil
	}
	t.Cleanup(func() { listOpenURLFunc = oldOpen })

	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		width:           100,
		height:          12,
		jobs:            nil,
	}
	m.rebuildGroupedRows()
	layout := m.buildGroupedViewLayout(m.groupedRows, m.groupedJobsWithAutoReasons())
	if layout.vastCreditWarningY < 0 {
		t.Fatal("expected actionable Vast.ai credit warning row")
	}

	next, cmd := m.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		Y:      layout.vastCreditWarningY,
	})
	got := next.(listTUIModel)
	if got.statusMessage != "Opening Vast.ai billing..." {
		t.Fatalf("statusMessage = %q, want open progress", got.statusMessage)
	}
	if cmd == nil {
		t.Fatal("expected open command")
	}
	msg, ok := cmd().(listURLOpenedMsg)
	if !ok {
		t.Fatalf("open command returned %T", msg)
	}
	if msg.url != vastaiBillingURL {
		t.Fatalf("opened url = %q, want %q", msg.url, vastaiBillingURL)
	}
	if len(opened) != 1 || opened[0] != vastaiBillingURL {
		t.Fatalf("opened = %v, want [%q]", opened, vastaiBillingURL)
	}

	next, _ = got.Update(msg)
	got = next.(listTUIModel)
	if got.statusMessage != "Opened Vast.ai billing" {
		t.Fatalf("statusMessage = %q, want success", got.statusMessage)
	}
}

func TestListTUIMouseClickFlatVastCreditWarningOpensBilling(t *testing.T) {
	SeedProviderCreditWarningForTesting(t, "WARNING: Vast.ai credits low ($5.00 < $10.00)")
	var opened []string
	oldOpen := listOpenURLFunc
	listOpenURLFunc = func(url string) error {
		opened = append(opened, url)
		return nil
	}
	t.Cleanup(func() { listOpenURLFunc = oldOpen })

	m := listTUIModel{
		title:  "Jobs",
		width:  100,
		height: 12,
		jobs: []*db.Job{
			{ID: 101, Status: db.StatusQueued, Description: "queued"},
		},
	}
	warningY, actionable := m.flatVastCreditWarningY()
	if !actionable || warningY < 0 {
		t.Fatal("expected actionable Vast.ai credit warning row")
	}

	next, cmd := m.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		Y:      warningY,
	})
	got := next.(listTUIModel)
	if got.statusMessage != "Opening Vast.ai billing..." {
		t.Fatalf("statusMessage = %q, want open progress", got.statusMessage)
	}
	if cmd == nil {
		t.Fatal("expected open command")
	}
	msg, ok := cmd().(listURLOpenedMsg)
	if !ok {
		t.Fatalf("open command returned %T", msg)
	}
	if msg.url != vastaiBillingURL {
		t.Fatalf("opened url = %q, want %q", msg.url, vastaiBillingURL)
	}
	if len(opened) != 1 || opened[0] != vastaiBillingURL {
		t.Fatalf("opened = %v, want [%q]", opened, vastaiBillingURL)
	}
}

func TestListTUIMouseClickCreditWarningIgnoresNonVastLowCredit(t *testing.T) {
	for _, warning := range []string{
		"WARNING: RunPod credits low ($5.00 < $10.00)",
		"WARNING: Vast.ai credit check failed (owner: Extra inputs are not permitted)",
	} {
		t.Run(warning, func(t *testing.T) {
			SeedProviderCreditWarningForTesting(t, warning)
			calls := 0
			oldOpen := listOpenURLFunc
			listOpenURLFunc = func(url string) error {
				calls++
				return nil
			}
			t.Cleanup(func() { listOpenURLFunc = oldOpen })

			m := listTUIModel{
				title:  "Jobs",
				width:  100,
				height: 12,
				jobs: []*db.Job{
					{ID: 101, Status: db.StatusQueued, Description: "queued"},
				},
			}
			_, actionable := m.flatVastCreditWarningY()
			if actionable {
				t.Fatalf("warning %q should not be actionable", warning)
			}
			_, cmd := m.Update(tea.MouseMsg{
				Button: tea.MouseButtonLeft,
				Action: tea.MouseActionPress,
				Y:      10,
			})
			if cmd != nil {
				t.Fatalf("warning %q produced command", warning)
			}
			if calls != 0 {
				t.Fatalf("open calls = %d, want 0", calls)
			}
		})
	}
}

func TestListTUIOpenURLFailureMessage(t *testing.T) {
	m := listTUIModel{}
	next, _ := m.Update(listURLOpenedMsg{url: vastaiBillingURL, err: errors.New("boom")})
	got := next.(listTUIModel)
	if got.statusMessage != "Open browser failed: boom" {
		t.Fatalf("statusMessage = %q, want failure", got.statusMessage)
	}
}

func TestListTUIMouseClickSelectsGroupedJobAfterBlockedReason(t *testing.T) {
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		width:           100,
		height:          20,
		jobs: []*db.Job{
			{ID: 101, Status: db.StatusQueued, Description: "blocked", QueueBlockedReason: "waiting for sibling"},
			{ID: 102, Status: db.StatusQueued, Description: "plain"},
		},
	}
	m.rebuildGroupedRows()
	clickY := groupedClickYForJob(t, m, 102)

	next, _ := m.Update(tea.MouseMsg{
		Button: tea.MouseButtonLeft,
		Action: tea.MouseActionPress,
		Y:      clickY,
	})
	got := next.(listTUIModel)
	if job := got.selectedGroupedJob(); job == nil || job.ID != 102 {
		t.Fatalf("selected grouped job = %+v, want wj102", job)
	}
}

// withStoppedDaemonStatus makes View() render the "Daemon: stopped" line by
// pinning the status probe to a not-live daemon. It drives daemonStatusProbe
// directly (rather than seeding filesystem fixtures) so it composes with the
// hermetic TestMain default — see main_test.go.
func withStoppedDaemonStatus(t *testing.T) {
	t.Helper()
	prev := daemonStatusProbe
	daemonStatusProbe = func(daemoncontrol.Paths) (daemoncontrol.Status, error) {
		return daemoncontrol.Status{}, nil
	}
	t.Cleanup(func() { daemonStatusProbe = prev })
}

// withStaleDaemonStatus makes View()/tick see a live daemon running a stale
// binary (the "Daemon: stale binary" / auto-restart path). The stale-detection
// logic itself is covered by the daemoncontrol package's own tests; here we
// only assert the TUI's reaction to a stale status.
func withStaleDaemonStatus(t *testing.T) {
	t.Helper()
	prev := daemonStatusProbe
	daemonStatusProbe = func(daemoncontrol.Paths) (daemoncontrol.Status, error) {
		return daemoncontrol.Status{PID: os.Getpid(), Live: true, ActiveBinaryStale: true}, nil
	}
	t.Cleanup(func() { daemonStatusProbe = prev })
}

func groupedClickYForJob(t *testing.T, m listTUIModel, jobID int64) int {
	t.Helper()
	for i, row := range m.groupedViewportRows() {
		if row.rowIdx < 0 || row.rowIdx >= len(m.groupedRows) {
			continue
		}
		job := m.groupedRows[row.rowIdx].job
		if job != nil && job.ID == jobID {
			return i + 1
		}
	}
	t.Fatalf("job %d was not visible in grouped viewport rows: %+v", jobID, m.groupedViewportRows())
	return 0
}

func TestListTUIFlatViewMarksSelectedRowWhenHostMatesActive(t *testing.T) {
	jobs := []*db.Job{
		{ID: 101, Host: "cool30", Status: db.StatusRunning, Description: "selected"},
		{ID: 102, Host: "cool30", Status: db.StatusQueued, Description: "mate"},
		{ID: 103, Host: "cool100", Status: db.StatusQueued, Description: "other"},
	}
	m := listTUIModel{
		title:  "Jobs",
		jobs:   jobs,
		layout: newJobListLayout(100, jobs, nil, false),
		cursor: 0,
		width:  100,
		height: 12,
	}

	out := stripANSI(m.View())
	lines := strings.Split(out, "\n")
	selectedLine := lineContaining(t, lines, "wj101")
	mateLine := lineContaining(t, lines, "wj102")
	otherLine := lineContaining(t, lines, "wj103")

	if !strings.HasPrefix(selectedLine, hostMateMarker) {
		t.Fatalf("selected row should show host-mate marker when mates are active, got: %q", selectedLine)
	}
	if !strings.HasPrefix(mateLine, hostMateMarker) {
		t.Fatalf("mate row should show host-mate marker, got: %q", mateLine)
	}
	if strings.HasPrefix(otherLine, hostMateMarker) {
		t.Fatalf("unrelated row should not show host-mate marker, got: %q", otherLine)
	}
}

func TestListTUIGroupedViewMarksSelectedRowWhenHostMatesActive(t *testing.T) {
	m := listTUIModel{
		title:           "Jobs",
		groupedByStatus: true,
		width:           100,
		height:          20,
		overloadedHostsByName: map[string]bool{
			"cool30": true,
		},
		jobs: []*db.Job{
			{ID: 101, Host: "cool30", Status: db.StatusRunning, Description: "selected"},
			{ID: 102, Host: "cool30", Status: db.StatusRunning, Description: "mate"},
			{ID: 103, Host: "cool100", Status: db.StatusQueued, Description: "other"},
		},
	}
	m.rebuildGroupedRows()

	out := stripANSI(m.View())
	if strings.Contains(out, "[38;5;") {
		t.Fatalf("grouped view exposed ANSI parameters after stripping:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	selectedLine := lineContaining(t, lines, "wj101")
	mateLine := lineContaining(t, lines, "wj102")
	otherLine := lineContaining(t, lines, "wj103")

	if !strings.HasPrefix(selectedLine, hostMateMarker) {
		t.Fatalf("selected row should show host-mate marker when mates are active, got: %q", selectedLine)
	}
	if !strings.HasPrefix(mateLine, hostMateMarker) {
		t.Fatalf("mate row should show host-mate marker, got: %q", mateLine)
	}
	if strings.HasPrefix(otherLine, hostMateMarker) {
		t.Fatalf("unrelated row should not show host-mate marker, got: %q", otherLine)
	}
}

func lineContaining(t *testing.T, lines []string, needle string) string {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("missing line containing %q in:\n%s", needle, strings.Join(lines, "\n"))
	return ""
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

func TestListTUIGroupedKeyPTogglesSelectedJobProcessedOff(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "queued")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.AddJobTag(database, jobID, db.ProcessedTag); err != nil {
		t.Fatalf("AddJobTag: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
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
		t.Fatal("expected command to toggle selected job processed tag")
	}
	if !strings.Contains(got.statusMessage, "unprocessed") {
		t.Fatalf("statusMessage = %q, want unprocessed text", got.statusMessage)
	}

	msg := cmd()
	next2, reloadCmd := got.Update(msg)
	got2 := next2.(listTUIModel)
	if reloadCmd == nil {
		t.Fatal("expected reload command after processed toggle")
	}
	if !strings.Contains(got2.statusMessage, "marked as unprocessed") {
		t.Fatalf("statusMessage = %q, want unprocessed confirmation", got2.statusMessage)
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID updated: %v", err)
	}
	if updated == nil || updated.HasTag(db.ProcessedTag) {
		t.Fatalf("expected job %d not to have processed tag", jobID)
	}
}

func TestListTUIUngroupedShiftUTogglesUnprocessedFilter(t *testing.T) {
	m := listTUIModel{
		width:  100,
		height: 20,
		jobs:   []*db.Job{{ID: 1, Status: db.StatusQueued}},
	}

	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'U'}})
	got := next.(listTUIModel)
	if cmd == nil {
		t.Fatal("expected reload command")
	}
	if !got.unprocessedView || !got.groupedUnprocessedView {
		t.Fatalf("unprocessed flags = %v/%v, want true/true", got.unprocessedView, got.groupedUnprocessedView)
	}
	if !strings.Contains(got.statusMessage, "unprocessed") {
		t.Fatalf("statusMessage = %q, want unprocessed text", got.statusMessage)
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

func TestMovePickerViewShowsLoadingWithoutExistingOptions(t *testing.T) {
	picker := movePickerModel{
		active:       true,
		jobID:        1933,
		loadingNew:   true,
		status:       movePickerStatus(0, 0, 0, true),
		existingDone: true,
	}

	out := picker.View(100, 30)
	for _, want := range []string{
		"Move job #1933 to:",
		"EXISTING INSTANCES",
		"none available",
		"NEW INSTANCE",
		"waiting for cloud offers",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("picker view missing %q:\n%s", want, out)
		}
	}
}

func TestMovePickerViewOmitsSingleNewInstanceStrategy(t *testing.T) {
	picker := movePickerModel{
		active: true,
		jobID:  1934,
		options: []moveOption{
			{
				kind:        orchestration.OptionKindNew,
				isNew:       true,
				strategy:    bidding.StrategyCheap,
				gpuName:     "RTX 3090",
				waitTime:    8 * time.Minute,
				costPerHour: 0.20,
				eligible:    true,
			},
		},
		existingDone: true,
	}

	out := stripANSI(picker.View(100, 30))
	if strings.Contains(out, "cheap") {
		t.Fatalf("single new-instance strategy should be omitted:\n%s", out)
	}
	if !strings.Contains(out, "RTX 3090") {
		t.Fatalf("picker view missing GPU:\n%s", out)
	}
}

func TestMovePickerViewShowsExistingSearchAfterCloudArrivesFirst(t *testing.T) {
	picker := movePickerModel{
		active: true,
		jobID:  1934,
		options: []moveOption{
			{
				kind:        orchestration.OptionKindNew,
				isNew:       true,
				gpuName:     "RTX 3090",
				waitTime:    8 * time.Minute,
				costPerHour: 0.20,
				eligible:    true,
			},
		},
		loadingNew:   false,
		existingDone: false,
		status:       movePickerStatus(0, 1, 0, false),
	}

	out := stripANSI(picker.View(100, 30))
	for _, want := range []string{"EXISTING INSTANCES", "searching destinations", "NEW INSTANCE", "RTX 3090"} {
		if !strings.Contains(out, want) {
			t.Fatalf("picker view missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "waiting for cloud offers") {
		t.Fatalf("cloud loader should be hidden after cloud arrives:\n%s", out)
	}
}

func TestListTUIGroupedEscCancelsActiveMoveLookup(t *testing.T) {
	m := listTUIModel{
		groupedByStatus:     true,
		width:               100,
		height:              20,
		moveLookupPending:   true,
		moveLookupRequestID: 7,
		movePicker: movePickerModel{
			active:    true,
			jobID:     12,
			requestID: 7,
		},
	}

	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := next.(listTUIModel)
	if got.moveLookupPending {
		t.Fatalf("moveLookupPending = true, want false")
	}
	if got.movePicker.active {
		t.Fatalf("move picker should be closed")
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

func TestListTUIGroupedAddsNewMoveOptionsToActivePicker(t *testing.T) {
	m := listTUIModel{
		groupedByStatus:     true,
		width:               100,
		height:              20,
		moveLookupPending:   true,
		moveLookupRequestID: 9,
		movePicker: movePickerModel{
			active:       true,
			jobID:        12,
			requestID:    9,
			options:      []moveOption{{kind: orchestration.OptionKindExisting, instanceID: 2871, gpuName: "RTX 4070S Ti", eligible: true}},
			loadingNew:   true,
			status:       movePickerStatus(1, 0, 0, true),
			existingDone: true,
		},
	}

	next, _ := m.Update(listMoveOptionsReadyMsg{
		requestID: 9,
		jobID:     12,
		options:   []moveOption{{kind: orchestration.OptionKindNew, isNew: true, gpuName: "A100", eligible: true}},
		newOnly:   true,
	})
	got := next.(listTUIModel)
	if got.moveLookupPending {
		t.Fatalf("moveLookupPending = true, want false")
	}
	if !got.movePicker.active {
		t.Fatalf("move picker should remain open")
	}
	if got.movePicker.loadingNew {
		t.Fatalf("loadingNew = true, want false")
	}
	if len(got.movePicker.options) != 2 {
		t.Fatalf("options len = %d, want 2", len(got.movePicker.options))
	}
	if !strings.Contains(got.movePicker.status, "Found 1 existing destination and 1 new destination") {
		t.Fatalf("status = %q", got.movePicker.status)
	}
}

func TestListTUIGroupedExistingRefreshDoesNotRestartCloudLoading(t *testing.T) {
	m := listTUIModel{
		groupedByStatus:   true,
		width:             100,
		height:            20,
		moveLookupPending: false,
		movePicker: movePickerModel{
			active:       true,
			jobID:        12,
			requestID:    9,
			options:      []moveOption{{kind: orchestration.OptionKindNew, isNew: true, gpuName: "RTX 3090", eligible: true}},
			loadingNew:   false,
			status:       movePickerStatus(0, 1, 0, false),
			existingDone: true,
		},
		jobs: []*db.Job{{ID: 12, Status: db.StatusQueued}},
	}

	next, _ := m.Update(listMoveOptionsReadyMsg{
		requestID:  9,
		jobID:      12,
		options:    []moveOption{{kind: orchestration.OptionKindOnPrem, host: "cool30", gpuName: "RTX 3090", eligible: true}},
		loadingNew: true,
	})
	got := next.(listTUIModel)
	if got.movePicker.loadingNew {
		t.Fatalf("late existing refresh restarted cloud loading")
	}
	out := stripANSI(got.movePicker.View(100, 30))
	if strings.Contains(out, "waiting for cloud offers") {
		t.Fatalf("late existing refresh should not show cloud loader:\n%s", out)
	}
	if !strings.Contains(out, "cool30") || !strings.Contains(out, "RTX 3090") {
		t.Fatalf("picker should keep existing and new rows:\n%s", out)
	}
}

func TestMovePickerViewShowsDisabledDestinations(t *testing.T) {
	picker := movePickerModel{
		active: true,
		jobID:  1936,
		options: []moveOption{
			{
				kind:       orchestration.OptionKindOnPrem,
				host:       "host-alpha",
				gpuName:    "A100 80GB PCIe",
				queueDepth: 0,
				eligible:   true,
			},
			{
				kind:       orchestration.OptionKindExisting,
				instanceID: 2871,
				gpuName:    "M2 Max",
				eligible:   false,
				reason:     "requires nvidia GPU",
			},
		},
		existingDone: true,
	}

	out := stripANSI(picker.View(100, 30))
	for _, want := range []string{
		"EXISTING INSTANCES",
		"host-alpha",
		"wi2871",
		"requires nvidia GPU",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("picker view missing %q:\n%s", want, out)
		}
	}
	picker.cursor = 1
	if got := picker.selectedOption(); got != nil {
		t.Fatalf("selected disabled option = %+v, want nil", *got)
	}
}

func TestMovePickerOnPremLineAbbreviatesGPUAndShowsLoad(t *testing.T) {
	line := formatMoveOptionLine(moveOption{
		kind:       orchestration.OptionKindOnPrem,
		host:       "cool30",
		gpuName:    "NVIDIA GeForce RTX 3090",
		queueDepth: 0,
		cpuPercent: 43,
		hasCPULoad: true,
		gpuPercent: 12,
		hasGPULoad: true,
		eligible:   true,
	})

	if strings.Contains(line, "NVIDIA GeForce") {
		t.Fatalf("line should abbreviate NVIDIA GeForce prefix: %q", line)
	}
	for _, want := range []string{"RTX 3090", "queue 0", "CPU 43%", "GPU 12%"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line missing %q: %q", want, line)
		}
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
	if !strings.Contains(out, "v group") {
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
	if !strings.Contains(out, "Selected job:") {
		t.Fatalf("expected selected job section, got:\n%s", out)
	}
	if !strings.Contains(out, "m move selected") {
		t.Fatalf("expected grouped move keybinding, got:\n%s", out)
	}
	if !strings.Contains(out, "e auto-pilot error details") {
		t.Fatalf("expected grouped error-details keybinding, got:\n%s", out)
	}
}

func TestListTUIHelpOverlayUsesTwoColumnsOnShortWideTerminal(t *testing.T) {
	m := listTUIModel{
		groupedByStatus: true,
		width:           100,
		height:          16,
		showHelp:        true,
	}

	out := stripANSI(m.View())
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) > m.height {
		t.Fatalf("help lines = %d, want <= %d:\n%s", len(lines), m.height, out)
	}
	for _, line := range lines {
		if lipgloss.Width(line) > m.width {
			t.Fatalf("line width = %d, want <= %d: %q", lipgloss.Width(line), m.width, line)
		}
	}
	if !strings.Contains(out, "Navigation:") || !strings.Contains(out, "Automation:") {
		t.Fatalf("expected two-column help to retain sections, got:\n%s", out)
	}
}
