package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/orchestration"
)

func TestFormatMoveExitSummaryAt_PrintsReceipt(t *testing.T) {
	now := time.Unix(1778230000, 0)
	result := orchestration.BulkResult{
		InstanceIDs:    []int64{2672},
		PlacedJobIDs:   []int64{1862, 1863},
		RequestedEach:  false,
		RequestedCount: 1,
	}
	out := FormatMoveExitSummaryAt(nil, result, now, 96)

	for _, want := range []string{
		"weft move ended - ",
		"Moved: wj1862, wj1863 → 1 new instance",
		"Instances:",
		"wi2672",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Next:") {
		t.Fatalf("summary should not include a Next line:\n%s", out)
	}
	if strings.Contains(out, "Placed ") {
		t.Fatalf("full-success summary should not include the shortfall header:\n%s", out)
	}
	if strings.Contains(out, "Unplaced") {
		t.Fatalf("full-success summary should not include any Unplaced line:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("summary should be plain text, got ANSI:\n%q", out)
	}
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if lipgloss.Width(line) > 96 {
			t.Fatalf("line width = %d, want <= 96: %q", lipgloss.Width(line), line)
		}
	}
}

// TestFormatMoveExitSummaryAt_ShortfallNamesUnplaced asserts the receipt
// surfaces a "Placed M of N distinct instances" header and per-reason
// Unplaced lines when --to distinct partially succeeded — the failure
// mode that motivated this commit (jobs silently dropped during launch).
func TestFormatMoveExitSummaryAt_ShortfallNamesUnplaced(t *testing.T) {
	now := time.Unix(1778230000, 0)
	result := orchestration.BulkResult{
		InstanceIDs:    []int64{3518, 3521, 3522},
		PlacedJobIDs:   []int64{2384, 2385, 2389},
		UnplacedJobs:   []orchestration.UnplacedJob{{JobID: 2383, Reason: "no offer accepted"}, {JobID: 2386, Reason: "no offer accepted"}, {JobID: 2387, Reason: "no offer accepted"}},
		RequestedEach:  true,
		RequestedCount: 6,
	}
	out := FormatMoveExitSummaryAt(nil, result, now, 120)

	for _, want := range []string{
		"Placed 3 of 6 distinct instances",
		"Moved:    wj2384, wj2385, wj2389",
		"Unplaced: wj2383, wj2386, wj2387 — no offer accepted",
		"Instances:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("shortfall receipt missing %q:\n%s", want, out)
		}
	}
	// The misleading old "Moved: <all 6 ids> → 3 new instances" line must
	// not appear in the shortfall path.
	if strings.Contains(out, "→ 3 new instances") || strings.Contains(out, "→ 3 distinct instances") {
		t.Fatalf("shortfall receipt should not pretend every job was moved:\n%s", out)
	}
}

// TestFormatMoveExitSummaryAt_ShortfallGroupsReasons asserts unplaced jobs
// sharing a reason collapse into one line so a multi-job stockout doesn't
// spam the receipt with N near-identical lines.
func TestFormatMoveExitSummaryAt_ShortfallGroupsReasons(t *testing.T) {
	now := time.Unix(1778230000, 0)
	result := orchestration.BulkResult{
		PlacedJobIDs: []int64{100},
		UnplacedJobs: []orchestration.UnplacedJob{
			{JobID: 101, Reason: "no offer accepted"},
			{JobID: 102, Reason: "no offer accepted"},
			{JobID: 103, Reason: "search offers: provider rejected"},
		},
		RequestedEach:  true,
		RequestedCount: 4,
	}
	out := FormatMoveExitSummaryAt(nil, result, now, 120)
	for _, want := range []string{
		"Placed 1 of 4 distinct instances",
		"Unplaced: wj101, wj102 — no offer accepted",
		"Unplaced: wj103 — search offers: provider rejected",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("grouped shortfall receipt missing %q:\n%s", want, out)
		}
	}
}

func TestFormatMoveExitSummaryAt_PrintsAlignedInstanceDetails(t *testing.T) {
	database := db.SetupTestDB(t)
	firstInstanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		ResolvedGPUName:  "RTX A6000",
		GPUMemGB:         48,
		CPUCores:         24,
		DiskGB:           120,
		CostPerHourCents: 64,
	})
	if err != nil {
		t.Fatalf("CreateLaunch(first): %v", err)
	}
	secondInstanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:           db.LaunchStatusRunning,
		Provider:         "vastai",
		ResolvedGPUName:  "Tesla V100",
		GPUMemGB:         32,
		CPUCores:         8,
		DiskGB:           80,
		CostPerHourCents: 21,
	})
	if err != nil {
		t.Fatalf("CreateLaunch(second): %v", err)
	}

	for _, jobID := range []int64{1894, 1895} {
		if err := db.RecordQueuedWithGPUAndID(database, jobID, "", "/tmp/markov-attention", "echo train", "", ""); err != nil {
			t.Fatalf("RecordQueuedWithGPUAndID(%d): %v", jobID, err)
		}
		if err := db.SetJobLaunchID(database, jobID, firstInstanceID); err != nil {
			t.Fatalf("SetJobLaunchID(%d, first): %v", jobID, err)
		}
	}
	if err := db.RecordQueuedWithGPUAndID(database, 1893, "", "/tmp/markov-attention", "echo train", "", ""); err != nil {
		t.Fatalf("RecordQueuedWithGPUAndID(1893): %v", err)
	}
	if err := db.SetJobLaunchID(database, 1893, secondInstanceID); err != nil {
		t.Fatalf("SetJobLaunchID(1893, second): %v", err)
	}

	now := time.Unix(1778230000, 0)
	result := orchestration.BulkResult{
		InstanceIDs:    []int64{firstInstanceID, secondInstanceID},
		PlacedJobIDs:   []int64{1894, 1893},
		RequestedEach:  false,
		RequestedCount: 2,
	}
	out := FormatMoveExitSummaryAt(database, result, now, 120)
	if strings.Contains(out, "new instances\n\nInstances:") {
		t.Fatalf("summary should not contain a blank line between Moved and Instances:\n%s", out)
	}
	for _, want := range []string{
		"RTX A6000 48GB",
		"Tesla V100 32GB",
		"24c",
		"8c",
		"120GB",
		"80GB",
		"$0.64/hr",
		"$0.21/hr",
		"running",
		"wj1894+",
		"wj1893",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q:\n%s", want, out)
		}
	}

	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	firstLine := findLineContaining(t, lines, ids.FormatInstanceID(firstInstanceID))
	secondLine := findLineContaining(t, lines, ids.FormatInstanceID(secondInstanceID))
	for _, cell := range []string{"running", "$0."} {
		if strings.Index(firstLine, cell) != strings.Index(secondLine, cell) {
			t.Fatalf("%q column not aligned:\n%s\n%s", cell, firstLine, secondLine)
		}
	}
}

func findLineContaining(t *testing.T, lines []string, want string) string {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("line containing %q not found in:\n%s", want, strings.Join(lines, "\n"))
	return ""
}
