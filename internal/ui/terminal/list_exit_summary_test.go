package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/db"
)

func TestListTUIExitSummaryAt_PrintsGroupedPlainReceipt(t *testing.T) {
	launchID := int64(2663)
	now := time.Unix(1778230000, 0)
	model := listTUIModel{
		title:           "Jobs - unprocessed",
		groupedByStatus: true,
		jobs: []*db.Job{
			{
				ID:          707,
				Status:      db.StatusRunning,
				Project:     "head-type-ontology",
				Description: "baseline sweep",
				LaunchID:    &launchID,
			},
			{
				ID:          1841,
				Status:      db.StatusQueued,
				Project:     "role-encoding-injection",
				Description: "KL probe",
				LaunchID:    &launchID,
			},
			{
				ID:          1241,
				Status:      db.StatusPendingPlacement,
				Project:     "llm-performance-models",
				Description: "V100 job",
			},
		},
		launchStatusByID: map[int64]string{launchID: db.LaunchStatusRunning},
		launchLiveByID: map[int64]*db.LaunchLiveState{
			launchID: {LaunchID: launchID, InstancePhase: "running:707", JobProgressID: 707, JobProgressPct: 42},
		},
		launchByID: map[int64]*db.Launch{
			launchID: {ID: launchID, Status: db.LaunchStatusRunning, ResolvedGPUName: "RTX A6000", GPUMemGB: 48},
		},
		recentFailedInstances: &recentFailedInstances{
			items: []*db.Launch{{ID: 9901, Status: db.LaunchStatusFailed, CreatedAt: now.Add(-time.Hour).Unix()}},
		},
	}

	out := model.exitSummaryAt(now, 96)
	for _, want := range []string{
		"Group: status",
		"Order: job id",
		"Filter: none",
		"Running (1):",
		"wj707",
		"baseline sweep",
		"Queued (1):",
		"wj1841",
		"Unplaced (1):",
		"wj1241",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{
		"weft uj ended",
		"Selected:",
		"Next:",
		"Recent failed instances",
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("summary should not include %q:\n%s", unwanted, out)
		}
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
