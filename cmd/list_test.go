package cmd

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
)

func TestFilterJobsByEffectiveStatusExcludesHostlessRunningFromRunning(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: "host-a"},
		{ID: 2, Status: db.StatusRunning, Host: ""},
		{ID: 3, Status: db.StatusQueued, Host: ""},
	}

	filtered := jobsWithEffectiveStatus(jobs, db.StatusRunning)
	if len(filtered) != 1 {
		t.Fatalf("expected 1 running job after effective filter, got %d", len(filtered))
	}
	if filtered[0].ID != 1 {
		t.Fatalf("expected job 1 to remain, got job %d", filtered[0].ID)
	}
}

func TestJobsWithEffectiveStatusReclassifiesHostlessRunningAsQueued(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: ""},
		{ID: 2, Status: db.StatusStarting, Host: ""},
		{ID: 3, Status: db.StatusQueued, Host: ""},
		{ID: 4, Status: db.StatusRunning, Host: "host-a"},
	}

	filtered := jobsWithEffectiveStatus(jobs, db.StatusQueued)
	if len(filtered) != 3 {
		t.Fatalf("expected 3 queued jobs after effective reclassification, got %d", len(filtered))
	}
}

func TestPrintJobsShowsDirectoryTailColumn(t *testing.T) {
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

	output := captureStdout(t, func() {
		if err := printJobs(jobs); err != nil {
			t.Fatalf("printJobs: %v", err)
		}
	})

	if !strings.Contains(output, "DIR") {
		t.Fatalf("output missing DIR header, got:\n%s", output)
	}
	if !strings.Contains(output, "project-alpha") {
		t.Fatalf("output missing directory tail, got:\n%s", output)
	}
}

func TestRenderJobListPlainTruncatesToWidth(t *testing.T) {
	jobs := []*db.Job{
		{
			ID:          42,
			Host:        "studio",
			Status:      db.StatusCompleted,
			StartTime:   1,
			WorkingDir:  "/workspace/project-alpha-with-a-long-name",
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
}

func TestRenderJobListPlainIncludesDirectoryOnWideTerminals(t *testing.T) {
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

	out := renderJobListPlain(jobs, 120)
	if !strings.Contains(out, "DIR") {
		t.Fatalf("output missing DIR header, got:\n%s", out)
	}
	if !strings.Contains(out, "project-alpha") {
		t.Fatalf("output missing directory tail, got:\n%s", out)
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

func testIntPtr(v int) *int {
	return &v
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stdout = w
	defer func() {
		os.Stdout = oldStdout
		_ = w.Close()
		_ = r.Close()
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close write pipe: %v", err)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read captured stdout: %v", err)
	}

	return string(data)
}
