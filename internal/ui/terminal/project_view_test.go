package terminal

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestGroupJobsByProjectUsesStoredProjectAndFallback(t *testing.T) {
	groups := groupJobsByProject([]*db.Job{
		{ID: 1, Project: "EXP-ALPHA", WorkingDir: "/tmp/project-a"},
		{ID: 2, WorkingDir: "/tmp/project-z"},
		{ID: 3, Project: "EXP-ALPHA", Command: "cd /tmp/project-b && python train.py"},
	})

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(groups))
	}
	if groups[0].Label != "EXP-ALPHA" {
		t.Fatalf("first group label = %q", groups[0].Label)
	}
	if strings.Join(groups[0].Directories, ",") != "/tmp/project-a,/tmp/project-b" {
		t.Fatalf("group dirs = %v", groups[0].Directories)
	}
	if groups[1].Label != "project-z" {
		t.Fatalf("fallback project label = %q", groups[1].Label)
	}
}

func TestRenderProjectJobsPlainShowsProjectBlocks(t *testing.T) {
	out := renderProjectJobsPlain([]projectGroup{
		{
			Label:       "EXP-ALPHA",
			Directories: []string{"/tmp/project-alpha"},
			Jobs: []*db.Job{
				{ID: 42, Host: "cool30", Status: db.StatusRunning, WorkingDir: "/tmp/project-alpha", Description: "train model"},
			},
		},
		{
			Label:       "EXP-BETA",
			Directories: []string{"/tmp/project-beta"},
			Jobs: []*db.Job{
				{ID: 43, Host: "cool100", Status: db.StatusQueued, WorkingDir: "/tmp/project-beta", Description: "eval model"},
			},
		},
	}, 120)

	for _, want := range []string{"EXP-ALPHA", "dir: /tmp/project-alpha", "EXP-BETA", "dir: /tmp/project-beta", "DESCRIPTION"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "\n\nEXP-BETA") {
		t.Fatalf("expected blank line between project blocks, got:\n%s", out)
	}
}

func TestGroupProjectActivityBucketsQueuedRunningAndRecent(t *testing.T) {
	end := time.Now().Unix()
	groups := groupProjectActivity(
		[]*db.Job{
			{ID: 1, Status: db.StatusRunning, Host: "cool30", WorkingDir: "/tmp/project-alpha", Project: "ALPHA", StartTime: 10},
			{ID: 2, Status: db.StatusRunning, Host: "", WorkingDir: "/tmp/project-alpha", Project: "ALPHA"},
			{ID: 3, Status: db.StatusPendingPlacement, WorkingDir: "/tmp/project-alpha", Project: "ALPHA"},
		},
		[]*db.Job{
			{ID: 4, Status: db.StatusCompleted, WorkingDir: "/tmp/project-alpha", Project: "ALPHA", EndTime: &end, ExitCode: testIntPtr(0)},
		},
	)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group, got %d", len(groups))
	}
	group := groups[0]
	if len(group.Running) != 1 {
		t.Fatalf("running count = %d, want 1", len(group.Running))
	}
	if len(group.Queued) != 2 {
		t.Fatalf("queued count = %d, want 2", len(group.Queued))
	}
	if len(group.Recent) != 1 {
		t.Fatalf("recent count = %d, want 1", len(group.Recent))
	}
}

func TestRenderProjectWatchPlainShowsSections(t *testing.T) {
	now := time.Unix(200, 0)
	end := int64(150)
	launchedAt := int64(0)
	out := renderProjectWatchPlain([]projectGroup{
		{
			Label:       "ALPHA",
			Directories: []string{"/tmp/project-alpha"},
			Running: []*db.Job{
				{ID: 1, Status: db.StatusRunning, Host: "cool30", WorkingDir: "/tmp/project-alpha", Description: "train", StartTime: 100},
			},
			Queued: []*db.Job{
				{ID: 2, Status: db.StatusQueued, WorkingDir: "/tmp/project-alpha", Description: "eval", QueuedAt: 120},
			},
			CloudInsts: []*db.Launch{
				{ID: 21, Status: db.LaunchStatusRunning, GPUSpec: "A100", CostPerHourCents: 150, LaunchedAt: &launchedAt},
			},
			Recent: []*db.Job{
				{ID: 3, Status: db.StatusFailed, Host: "cool30", WorkingDir: "/tmp/project-alpha", Description: "old", EndTime: &end},
			},
		},
	}, 120, now, 24*time.Hour)

	for _, want := range []string{"ALPHA (1 running, 1 queued, 1 recent/1d)", "Running", "Queued", "Rental instances", "Recent", "#1", "#2", "#3", "instance 21", "rate: $1.50/hr"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestProjectWatchModelViewShowsGroupedContent(t *testing.T) {
	end := int64(time.Now().Unix())
	groups := []projectGroup{
		{
			Label:       "ALPHA",
			Directories: []string{"/tmp/project-alpha"},
			Running: []*db.Job{
				{ID: 1, Status: db.StatusRunning, Host: "cool30", WorkingDir: "/tmp/project-alpha", Description: "train", StartTime: time.Now().Add(-2 * time.Hour).Unix()},
			},
			Recent: []*db.Job{
				{ID: 2, Status: db.StatusCompleted, Host: "cool30", WorkingDir: "/tmp/project-alpha", Description: "done", EndTime: &end, ExitCode: testIntPtr(0)},
			},
		},
	}

	m := watchModel{
		mode:           watchModeProject,
		width:          120,
		height:         12,
		projectRecent:  24 * time.Hour,
		projectGroups:  groups,
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		jobProgressHWM: map[int64]int{},
	}
	m.projectLines, m.projectMeta = m.computeProjectLines()

	out := stripANSI(m.View())
	for _, want := range []string{"Project Watch (1 projects)", "ALPHA", "Running", "Recent", "r refresh"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q, got:\n%s", want, out)
		}
	}
}

func TestProjectWatchFooterShowsSelectedBlockedReason(t *testing.T) {
	blocked := &db.Job{
		ID:                 41,
		Status:             db.StatusQueued,
		Host:               "cool30",
		WorkingDir:         "/tmp/project-alpha",
		Description:        "blocked job",
		QueueBlockedReason: "cpu gate: 80% + 40% > 90% target",
	}
	queued := &db.Job{
		ID:          42,
		Status:      db.StatusQueued,
		Host:        "cool30",
		WorkingDir:  "/tmp/project-alpha",
		Description: "next job",
	}
	m := watchModel{
		mode:          watchModeProject,
		width:         180,
		height:        12,
		projectRecent: 24 * time.Hour,
		projectGroups: []projectGroup{
			{
				Label:       "ALPHA",
				Directories: []string{"/tmp/project-alpha"},
				Queued:      []*db.Job{blocked, queued},
			},
		},
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		jobProgressHWM: map[int64]int{},
	}
	m.projectLines, m.projectMeta = m.computeProjectLines()
	m.cursor = 3 // header + dir + "Queued" + first job row

	out := stripANSI(m.View())
	if !strings.Contains(out, "#41 blocked: cpu gate: 80% + 40% > 90% target") {
		t.Fatalf("footer missing blocked detail, got:\n%s", out)
	}
}

func TestProjectWatchFooterOmitsBlockedReasonForNonBlockedSelection(t *testing.T) {
	blocked := &db.Job{
		ID:                 41,
		Status:             db.StatusQueued,
		Host:               "cool30",
		WorkingDir:         "/tmp/project-alpha",
		Description:        "blocked job",
		QueueBlockedReason: "cpu gate: 80% + 40% > 90% target",
	}
	queued := &db.Job{
		ID:          42,
		Status:      db.StatusQueued,
		Host:        "cool30",
		WorkingDir:  "/tmp/project-alpha",
		Description: "next job",
	}
	m := watchModel{
		mode:          watchModeProject,
		width:         180,
		height:        12,
		projectRecent: 24 * time.Hour,
		projectGroups: []projectGroup{
			{
				Label:       "ALPHA",
				Directories: []string{"/tmp/project-alpha"},
				Queued:      []*db.Job{blocked, queued},
			},
		},
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		jobProgressHWM: map[int64]int{},
	}
	m.projectLines, m.projectMeta = m.computeProjectLines()
	m.cursor = 4 // second queued row (not blocked)

	out := stripANSI(m.View())
	if strings.Contains(out, "#41 blocked:") {
		t.Fatalf("footer should not show blocked detail for non-blocked selection, got:\n%s", out)
	}
}

func TestProjectWatchFooterTruncatesBlockedReasonToWidth(t *testing.T) {
	m := watchModel{
		mode:          watchModeProject,
		width:         100,
		height:        12,
		projectRecent: 24 * time.Hour,
		projectGroups: []projectGroup{
			{
				Label:       "ALPHA",
				Directories: []string{"/tmp/project-alpha"},
				Queued: []*db.Job{
					{
						ID:                 41,
						Status:             db.StatusQueued,
						Host:               "cool30",
						WorkingDir:         "/tmp/project-alpha",
						Description:        "blocked job",
						QueueBlockedReason: "gpu gate: waiting for requested GPU capacity on device set 0,1,2,3",
					},
				},
			},
		},
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		jobProgressHWM: map[int64]int{},
	}
	m.projectLines, m.projectMeta = m.computeProjectLines()
	m.cursor = 3

	out := stripANSI(m.View())
	if !strings.Contains(out, "#41 blocked:") {
		t.Fatalf("footer missing blocked prefix, got:\n%s", out)
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("footer should include ellipsis for truncated detail, got:\n%s", out)
	}
}

func TestProjectWatchToggleHelpWithQuestionMark(t *testing.T) {
	m := watchModel{
		mode:          watchModeProject,
		width:         120,
		height:        12,
		projectRecent: 24 * time.Hour,
		projectGroups: []projectGroup{
			{
				Label:       "ALPHA",
				Directories: []string{"/tmp/project-alpha"},
			},
		},
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		jobProgressHWM: map[int64]int{},
	}
	m.projectLines, m.projectMeta = m.computeProjectLines()

	updatedModel, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	got := updatedModel.(watchModel)
	if !got.projectHelp {
		t.Fatalf("expected project help to be visible")
	}
	out := stripANSI(got.View())
	if !strings.Contains(out, "Project Watch Keybindings") {
		t.Fatalf("missing help title, got:\n%s", out)
	}
	if !strings.Contains(out, "u unplace selected queued job") {
		t.Fatalf("missing unplace help text, got:\n%s", out)
	}

	updatedModel, _ = got.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	got = updatedModel.(watchModel)
	if got.projectHelp {
		t.Fatalf("expected project help to close")
	}
}

func TestProjectWatchKeyUUnplacesSelectedQueuedJob(t *testing.T) {
	job := &db.Job{
		ID:          41,
		Status:      db.StatusQueued,
		Host:        "cool30",
		WorkingDir:  "/tmp/project-alpha",
		Description: "queued job",
	}
	m := watchModel{
		mode:          watchModeProject,
		width:         120,
		height:        12,
		projectRecent: 24 * time.Hour,
		projectGroups: []projectGroup{
			{
				Label:       "ALPHA",
				Directories: []string{"/tmp/project-alpha"},
				Queued:      []*db.Job{job},
			},
		},
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		jobProgressHWM: map[int64]int{},
	}
	m.projectLines, m.projectMeta = m.computeProjectLines()
	m.cursor = 3

	updatedModel, cmd := m.handleProjectKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	got := updatedModel.(watchModel)
	if cmd == nil {
		t.Fatalf("expected unplace command")
	}
	if !strings.Contains(got.flash.Message, "Unplacing job #41") {
		t.Fatalf("expected unplace flash, got %q", got.flash.Message)
	}
}

func TestProjectWatchKeyUOnUnplacedShowsMessage(t *testing.T) {
	job := &db.Job{
		ID:          41,
		Status:      db.StatusQueued,
		Host:        "",
		WorkingDir:  "/tmp/project-alpha",
		Description: "unplaced job",
	}
	m := watchModel{
		mode:          watchModeProject,
		width:         120,
		height:        12,
		projectRecent: 24 * time.Hour,
		projectGroups: []projectGroup{
			{
				Label:       "ALPHA",
				Directories: []string{"/tmp/project-alpha"},
				Unplaced:    []*db.Job{job},
			},
		},
		updates:        map[int64]campaign.InstanceUpdate{},
		channels:       map[int64]<-chan campaign.InstanceUpdate{},
		clients:        map[int64]cloud.Client{},
		jobProgressHWM: map[int64]int{},
	}
	m.projectLines, m.projectMeta = m.computeProjectLines()
	m.cursor = 3

	updatedModel, _ := m.handleProjectKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	got := updatedModel.(watchModel)
	if !strings.Contains(got.flash.Message, "already unplaced") {
		t.Fatalf("expected already unplaced message, got %q", got.flash.Message)
	}
}

func TestAttachProjectLaunchesDedupesWithinProject(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}

	groups := []projectGroup{
		{
			Label: "ALPHA",
			Running: []*db.Job{
				{ID: 1, Project: "ALPHA", LaunchID: &instanceID},
			},
			Recent: []*db.Job{
				{ID: 2, Project: "ALPHA", LaunchID: &instanceID},
			},
		},
	}

	if err := attachProjectLaunches(database, groups); err != nil {
		t.Fatalf("attachProjectLaunches: %v", err)
	}
	if len(groups[0].CloudInsts) != 1 {
		t.Fatalf("cloud instance count = %d, want 1", len(groups[0].CloudInsts))
	}
	if groups[0].CloudInsts[0].ID != instanceID {
		t.Fatalf("cloud instance id = %d, want %d", groups[0].CloudInsts[0].ID, instanceID)
	}
}
