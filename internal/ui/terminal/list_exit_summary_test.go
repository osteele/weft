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
		unprocessedView: true,
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
		"Unprocessed jobs · grouped by status · ordered by job id",
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
		"Group:",
		"Order:",
		"Filter:",
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

func TestListTUIExitSummaryHeader_ComposesOptionalClauses(t *testing.T) {
	tests := []struct {
		name  string
		model listTUIModel
		want  string
	}{
		{
			name:  "plain jobs",
			model: listTUIModel{},
			want:  "Jobs · ordered by current list order",
		},
		{
			name: "status grouped",
			model: listTUIModel{
				groupedByStatus: true,
			},
			want: "Jobs · grouped by status · ordered by job id",
		},
		{
			name: "unprocessed project",
			model: listTUIModel{
				unprocessedView: true,
				projectFilter:   "contour-pareto",
			},
			want: "Unprocessed jobs · project contour-pareto · ordered by current list order",
		},
		{
			name: "failed unprocessed grouped",
			model: listTUIModel{
				unprocessedView: true,
				statusView:      db.StatusFailed,
				groupMode:       listGroupStatus,
			},
			want: "Unprocessed failed jobs · grouped by status · ordered by job id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.model.exitSummaryHeader(); got != tt.want {
				t.Fatalf("header = %q, want %q", got, tt.want)
			}
		})
	}
}
