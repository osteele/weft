package cmd

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

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

func TestFilterJobsByHostFlag_StrictHostMatch(t *testing.T) {
	prevHost := listHost
	listHost = "cool30"
	t.Cleanup(func() { listHost = prevHost })

	jobs := []*db.Job{
		{ID: 1, Host: "cool30"},
		{ID: 2, Host: "rental:444"},
		{ID: 3, Host: ""},
		{ID: 4, Host: db.LaunchHost(17)},
	}

	filtered := filterJobsByHostFlag(jobs)
	if len(filtered) != 1 {
		t.Fatalf("expected only 1 host-matching job, got %d", len(filtered))
	}
	if filtered[0].ID != 1 {
		t.Fatalf("expected job 1 to remain, got job %d", filtered[0].ID)
	}
}

func TestFilterJobsByHostFlag_NoHostFlagReturnsInput(t *testing.T) {
	prevHost := listHost
	listHost = ""
	t.Cleanup(func() { listHost = prevHost })

	jobs := []*db.Job{
		{ID: 1, Host: "cool30"},
		{ID: 2, Host: "rental:444"},
		{ID: 3, Host: ""},
	}

	filtered := filterJobsByHostFlag(jobs)
	if len(filtered) != len(jobs) {
		t.Fatalf("expected unfiltered jobs, got %d", len(filtered))
	}
}

func TestFilterJobsSinceUsesLatestLifecycleTimestamp(t *testing.T) {
	cutoff := time.Unix(1_000, 0)
	end := int64(1_100)
	jobs := []*db.Job{
		{ID: 1, CreatedAt: 900, StartTime: 950},
		{ID: 2, CreatedAt: 800, EndTime: &end},
		{ID: 3, CreatedAt: 700, QueuedAt: 1_001},
	}

	filtered := filterJobsSince(jobs, cutoff)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 jobs since cutoff, got %d", len(filtered))
	}
	if filtered[0].ID != 2 || filtered[1].ID != 3 {
		t.Fatalf("unexpected filtered jobs: %+v", filtered)
	}
}

func TestFilterActiveJobsExcludesTerminalStatuses(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued},
		{ID: 2, Status: db.StatusRunning, Host: "cool30"},
		{ID: 3, Status: db.StatusCompleted},
		{ID: 4, Status: db.StatusFailed},
		{ID: 5, Status: db.StatusCanceled},
	}

	filtered := filterActiveJobs(jobs)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 active jobs, got %d", len(filtered))
	}
	if filtered[0].ID != 1 || filtered[1].ID != 2 {
		t.Fatalf("unexpected active jobs: %+v", filtered)
	}
}

func TestListFiltersStatusComposesWithUnprocessed(t *testing.T) {
	prevStatus := listStatus
	prevProcessed := listProcessed
	prevUnprocessed := listUnprocessed
	prevRunning := listRunning
	prevCompleted := listCompleted
	prevQueued := listQueued
	prevDead := listDead
	prevFailed := listFailed
	listStatus = db.StatusFailed
	listProcessed = false
	listUnprocessed = true
	listRunning = false
	listCompleted = false
	listQueued = false
	listDead = false
	listFailed = false
	t.Cleanup(func() {
		listStatus = prevStatus
		listProcessed = prevProcessed
		listUnprocessed = prevUnprocessed
		listRunning = prevRunning
		listCompleted = prevCompleted
		listQueued = prevQueued
		listDead = prevDead
		listFailed = prevFailed
	})

	statusFilter, processedFilter, failedOnly, err := listFilters()
	if err != nil {
		t.Fatalf("listFilters: %v", err)
	}
	if statusFilter != db.StatusFailed || processedFilter != "unprocessed" || failedOnly {
		t.Fatalf("filters = status %q processed %q failedOnly %v, want failed/unprocessed/false",
			statusFilter, processedFilter, failedOnly)
	}
}

func TestWriteWarningsDeduplicatesMessages(t *testing.T) {
	var b strings.Builder
	writeWarnings(&b, []string{"Warning: R2 storage unreachable", "Warning: R2 storage unreachable", "Warning: host slow"})

	out := b.String()
	if strings.Count(out, "Warning: R2 storage unreachable") != 1 {
		t.Fatalf("expected R2 warning once, got:\n%s", out)
	}
	if !strings.Contains(out, "Warning: host slow") {
		t.Fatalf("missing distinct warning, got:\n%s", out)
	}
}

func TestValidateListGroupingOptionsUnknownValue(t *testing.T) {
	prevGroupBy := listGroupBy
	prevFormat := listFormat
	listGroupBy = "bogus"
	listFormat = "table"
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
	})

	err := validateListGroupingOptions()
	if err == nil {
		t.Fatal("expected error for unknown group-by value")
	}
	if !strings.Contains(err.Error(), "supported: status, project") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateListGroupingOptionsRejectsJSON(t *testing.T) {
	prevGroupBy := listGroupBy
	prevFormat := listFormat
	listGroupBy = "status"
	listFormat = "json"
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
	})

	err := validateListGroupingOptions()
	if err == nil {
		t.Fatal("expected error for --group-by with json format")
	}
	if !strings.Contains(err.Error(), "table output only") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateListGroupingOptionsRejectsJSONForProject(t *testing.T) {
	prevGroupBy := listGroupBy
	prevFormat := listFormat
	listGroupBy = "project"
	listFormat = "json"
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
	})

	err := validateListGroupingOptions()
	if err == nil {
		t.Fatal("expected error for --group-by with json format")
	}
	if !strings.Contains(err.Error(), "table output only") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPrintJobsGroupedStatus(t *testing.T) {
	prevGroupBy := listGroupBy
	prevFormat := listFormat
	listGroupBy = "status"
	listFormat = "table"
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
	})

	jobs := []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: "cool30", Project: "proj", Description: "run"},
		{ID: 2, Status: db.StatusQueued, Host: "cool30", Project: "proj", Description: "wait"},
	}
	out := captureStdout(t, func() {
		if err := printJobs(nil, jobs); err != nil {
			t.Fatalf("printJobs: %v", err)
		}
	})

	if !strings.Contains(out, "Running (1):") {
		t.Fatalf("expected grouped running section, got:\n%s", out)
	}
	if !strings.Contains(out, "Queued (1):") {
		t.Fatalf("expected grouped queued section, got:\n%s", out)
	}
}

func TestPrintJobsGroupedProject(t *testing.T) {
	prevGroupBy := listGroupBy
	prevFormat := listFormat
	listGroupBy = "project"
	listFormat = "table"
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
	})

	jobs := []*db.Job{
		{ID: 1, Status: db.StatusRunning, Host: "cool30", Project: "proj-a", WorkingDir: "/tmp/proj-a", Description: "run"},
		{ID: 2, Status: db.StatusQueued, Host: "cool100", Project: "proj-b", WorkingDir: "/tmp/proj-b", Description: "wait"},
	}
	out := captureStdout(t, func() {
		if err := printJobs(nil, jobs); err != nil {
			t.Fatalf("printJobs: %v", err)
		}
	})

	for _, want := range []string{"proj-a", "proj-b", "dir: /tmp/proj-a", "dir: /tmp/proj-b", "DESCRIPTION"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestPrintJobsGroupedStatusUnprocessedExcludesCanceledKeepsKilled(t *testing.T) {
	prevGroupBy := listGroupBy
	prevFormat := listFormat
	prevUnprocessed := listUnprocessed
	prevProcessed := listProcessed
	prevStatus := listStatus
	listGroupBy = "status"
	listFormat = "table"
	listUnprocessed = true
	listProcessed = false
	listStatus = ""
	t.Cleanup(func() {
		listGroupBy = prevGroupBy
		listFormat = prevFormat
		listUnprocessed = prevUnprocessed
		listProcessed = prevProcessed
		listStatus = prevStatus
	})

	jobs := []*db.Job{
		{ID: 8, Status: db.StatusKilled, Host: "cool30", Project: "proj", Description: "killed"},
		{ID: 9, Status: db.StatusCanceled, Host: "cool30", Project: "proj", Description: "canceled"},
	}

	out := captureStdout(t, func() {
		if err := printJobs(nil, jobs); err != nil {
			t.Fatalf("printJobs: %v", err)
		}
	})

	if !strings.Contains(out, "Killed/Canceled (1):") {
		t.Fatalf("expected grouped killed/canceled section with one job, got:\n%s", out)
	}
	if !strings.Contains(out, "killed") {
		t.Fatalf("expected killed job line, got:\n%s", out)
	}
	if strings.Contains(out, "canceled") {
		t.Fatalf("did not expect canceled job line in grouped unprocessed output, got:\n%s", out)
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
