package cmd

import (
	"io"
	"os"
	"strings"
	"testing"

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
			Project:     "llm-performance-models",
			Description: "train model",
		},
	}

	output := captureStdout(t, func() {
		if err := printJobs(nil, jobs); err != nil {
			t.Fatalf("printJobs: %v", err)
		}
	})

	if !strings.Contains(output, "DIR") {
		t.Fatalf("output missing DIR header, got:\n%s", output)
	}
	if !strings.Contains(output, "PROJECT") {
		t.Fatalf("output missing PROJECT header, got:\n%s", output)
	}
	if !strings.Contains(output, "project-alpha") {
		t.Fatalf("output missing directory tail, got:\n%s", output)
	}
	if !strings.Contains(output, "llm-performance-models") {
		t.Fatalf("output missing project name, got:\n%s", output)
	}
}

func TestFilterJobsByPlacementScope_RentalMatchesTagOrCloudAssignment(t *testing.T) {
	cloudInstanceID := int64(17)
	jobs := []*db.Job{
		{ID: 1, Tags: []string{db.TagRental}},
		{ID: 2, Tags: []string{db.TagCloudLegacy}},
		{ID: 3, Host: db.LaunchHost(cloudInstanceID), LaunchID: &cloudInstanceID},
		{ID: 4, Host: "cool30"},
		{ID: 5, Tags: []string{db.TagInventory}},
	}

	filtered := filterJobsByPlacementScope(jobs, true, false)
	if len(filtered) != 3 {
		t.Fatalf("expected 3 rental jobs, got %d (%+v)", len(filtered), filtered)
	}
}

func TestFilterJobsByPlacementScope_InventoryMatchesInventoryTagOrInventoryHost(t *testing.T) {
	cloudInstanceID := int64(17)
	jobs := []*db.Job{
		{ID: 1, Tags: []string{db.TagInventory}},
		{ID: 2, Host: "cool30"},
		{ID: 3, Host: "", Tags: []string{db.TagInventory}},
		{ID: 4, Tags: []string{db.TagRental}},
		{ID: 5, Host: db.LaunchHost(cloudInstanceID), LaunchID: &cloudInstanceID},
		{ID: 6, Host: ""},
	}

	filtered := filterJobsByPlacementScope(jobs, false, true)
	if len(filtered) != 3 {
		t.Fatalf("expected 3 inventory jobs, got %d (%+v)", len(filtered), filtered)
	}
	for _, job := range filtered {
		if job.UsesRentalPlacement() {
			t.Fatalf("inventory filter should exclude rental jobs, got %+v", job)
		}
	}
}

func TestFilterJobsByPlacementScope_NoFilterReturnsInput(t *testing.T) {
	jobs := []*db.Job{{ID: 1}, {ID: 2}}
	filtered := filterJobsByPlacementScope(jobs, false, false)
	if len(filtered) != len(jobs) {
		t.Fatalf("expected unfiltered jobs, got %d", len(filtered))
	}
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
	}()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(data)
}
