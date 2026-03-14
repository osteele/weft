package cmd

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

func TestFilterJobsByFailureState(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusCompleted, ExitCode: testIntPtr(0)},
		{ID: 2, Status: db.StatusCompleted, ExitCode: testIntPtr(3)},
		{ID: 3, Status: db.StatusFailed},
		{ID: 4, Status: db.StatusDead},
		{ID: 5, Status: db.StatusKilled},
	}

	filtered := filterJobsByFailureState(jobs, true)
	if len(filtered) != 3 {
		t.Fatalf("expected 3 failed jobs, got %d", len(filtered))
	}
	if filtered[0].ID != 2 || filtered[1].ID != 3 || filtered[2].ID != 4 {
		t.Fatalf("unexpected failed job IDs: %+v", filtered)
	}
}

func TestProjectCommandsExposeSharedListFlags(t *testing.T) {
	for _, cmd := range []*cobra.Command{jobListCmd, projectJobsCmd} {
		for _, name := range []string{"failed", "processed", "unprocessed", "rental", "inventory", "cloud"} {
			if flag := cmd.Flags().Lookup(name); flag == nil {
				t.Fatalf("%s missing flag %q", cmd.Name(), name)
			}
		}
		if flag := cmd.Flags().Lookup("cloud"); flag != nil && !flag.Hidden {
			t.Fatalf("%s cloud alias flag should be hidden", cmd.Name())
		}
	}
}

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
			Recent: []*db.Job{
				{ID: 3, Status: db.StatusFailed, Host: "cool30", WorkingDir: "/tmp/project-alpha", Description: "old", EndTime: &end},
			},
		},
	}, 120, now, 24*time.Hour)

	for _, want := range []string{"ALPHA (1 running, 1 queued, 1 recent/1d)", "Running", "Queued", "Recent", "#1", "#2", "#3"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestProjectWatchModelViewShowsGroupedContent(t *testing.T) {
	end := int64(time.Now().Unix())
	m := projectWatchModel{
		width:        120,
		height:       12,
		recentWindow: 24 * time.Hour,
		groups: []projectGroup{
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
		},
	}

	out := stripANSI(m.View())
	for _, want := range []string{"Project Watch (1 projects)", "ALPHA", "Running", "Recent", "r refresh"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q, got:\n%s", want, out)
		}
	}
}
