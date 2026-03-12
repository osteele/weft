package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestWatchAllModelViewShowsSectionsAndDirectoryTails(t *testing.T) {
	cloudInstance := &db.CloudInstance{
		ID:       5,
		Status:   db.CloudInstanceStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	}

	m := watchAllModel{
		width:  120,
		height: 20,
		cloudInstances: []*db.CloudInstance{
			cloudInstance,
		},
		instanceUpdates: map[int64]campaign.InstanceUpdate{
			5: {
				CloudInstance: cloudInstance,
				Jobs: []*db.Job{
					{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/project-alpha", Description: "train model"},
					{ID: 89, Status: db.StatusQueued, WorkingDir: "/workspace/project-delta", Description: "eval model"},
				},
			},
		},
		onPremHosts: []onPremHostSummary{
			{
				Name: "cool30",
				Jobs: []*db.Job{
					{ID: 41, Status: db.StatusRunning, Host: "cool30", WorkingDir: "/tmp/project-beta", Description: "eval model"},
				},
			},
		},
		unplacedJobs: []*db.Job{
			{ID: 123, Status: db.StatusQueued, WorkingDir: "/tmp/project-gamma", Description: "benchmark", GPUClass: "A100"},
		},
	}

	out := stripANSI(m.View())
	for _, expected := range []string{
		"Cloud Instances (1)",
		"On-Prem Hosts (1 active)",
		"Unplaced Jobs (1)",
		"[u] unplace queued job",
		"Instance 5 — A100 — running",
		"  vastai:",
		"  Jobs: 0/2 resolved",
		"project-alpha",
		"project-delta",
		"project-beta",
		"project-gamma",
		"cool30",
	} {
		if !strings.Contains(out, expected) {
			t.Fatalf("output missing %q, got:\n%s", expected, out)
		}
	}
	if strings.Contains(out, "ID 5") {
		t.Fatalf("output should omit redundant provider line ID, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockShowsCampaignStyleLayout(t *testing.T) {
	ci := &db.CloudInstance{
		ID:                 5,
		Status:             db.CloudInstanceStatusRunning,
		Provider:           "vastai",
		ProviderInstanceID: "32734388",
		GPUSpec:            "A100",
	}
	update := campaign.InstanceUpdate{
		CloudInstance: ci,
		Jobs: []*db.Job{
			{ID: 88, Status: db.StatusRunning, WorkingDir: "/workspace/project-alpha", Description: "train model"},
			{ID: 89, Status: db.StatusQueued, WorkingDir: "/workspace/project-delta", Description: "eval model"},
		},
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{}))
	for _, expected := range []string{
		"Instance 5 — A100 — running",
		"  vastai: 32734388",
		"  Jobs: 0/2 resolved",
		"project-alpha",
		"project-delta",
	} {
		if !strings.Contains(out, expected) {
			t.Fatalf("output missing %q, got:\n%s", expected, out)
		}
	}
	if strings.Contains(out, "ID 5") {
		t.Fatalf("output should omit redundant provider line ID, got:\n%s", out)
	}
}

func TestFormatWatchInstanceBlockPrefersProviderLoadingStatus(t *testing.T) {
	ci := &db.CloudInstance{
		ID:                 111,
		Status:             db.CloudInstanceStatusRunning,
		Provider:           "vastai",
		ProviderInstanceID: "32740493",
		GPUSpec:            "A100",
	}
	update := campaign.InstanceUpdate{
		CloudInstance: ci,
		Instance:      &cloud.Instance{Status: "loading"},
	}

	out := stripANSI(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{}))
	if !strings.Contains(out, "Instance 111 — A100 — loading") {
		t.Fatalf("expected provider loading status in header, got:\n%s", out)
	}
}

func TestFormatOnPremJobRowQueuedUsesDashDuration(t *testing.T) {
	m := watchAllModel{}
	row := m.formatOnPremJobRow(&db.Job{
		ID:          41,
		Status:      db.StatusQueued,
		Host:        "cool30",
		WorkingDir:  "/tmp/project-beta",
		Description: "eval",
	})

	fields := strings.Fields(row)
	if len(fields) < 5 {
		t.Fatalf("row %q has too few fields", row)
	}
	if got := fields[len(fields)-2]; got != db.StatusQueued {
		t.Fatalf("status field = %q, want %q in row %q", got, db.StatusQueued, row)
	}
	if got := fields[len(fields)-1]; got != "—" {
		t.Fatalf("duration field = %q, want %q in row %q", got, "—", row)
	}
}

func TestWatchAllModelUnplaceDoneMovesJobImmediately(t *testing.T) {
	m := watchAllModel{
		onPremHosts: []onPremHostSummary{
			{
				Name: "cool30",
				Jobs: []*db.Job{
					{ID: 41, Status: db.StatusQueued, Host: "cool30", WorkingDir: "/tmp/project-beta", Description: "eval model"},
				},
			},
		},
	}

	msg := watchUnplaceDoneMsg{
		job: &db.Job{
			ID:          41,
			Status:      db.StatusQueued,
			Host:        "",
			WorkingDir:  "/tmp/project-beta",
			Description: "eval model",
			Tags:        []string{db.TagCloud},
		},
		message: "moved",
	}

	updatedModel, _ := m.Update(msg)
	got := updatedModel.(watchAllModel)

	if len(got.onPremHosts) != 0 {
		t.Fatalf("expected on-prem host list to be empty, got %+v", got.onPremHosts)
	}
	if len(got.unplacedJobs) != 1 {
		t.Fatalf("expected one unplaced job, got %+v", got.unplacedJobs)
	}
	if got.unplacedJobs[0].ID != 41 {
		t.Fatalf("unplaced job ID = %d, want 41", got.unplacedJobs[0].ID)
	}
	if got.unplacedJobs[0].Host != "" {
		t.Fatalf("unplaced job host = %q, want empty", got.unplacedJobs[0].Host)
	}
}

func TestWatchAllModelViewShowsSelectedUnplacedReasonInFooter(t *testing.T) {
	m := watchAllModel{
		width:  180,
		height: 12,
		cursor: 0,
		unplacedJobs: []*db.Job{
			{
				ID:               189,
				Status:           db.StatusQueued,
				WorkingDir:       "/tmp/project-gamma",
				Description:      "benchmark",
				GPUClass:         "L40s",
				PlacementReasons: []string{"no local host matched gpu-class=L40s, gpu-mem>=20GB", "2 hosts: no L40s GPU"},
			},
		},
	}

	out := stripANSI(m.View())
	for _, want := range []string{
		"#189 unplaced: no local host matched gpu-class=L40s, gpu-mem>=20GB | 2 hosts: no L40s GPU",
		"[u] unplace queued job",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestWatchAllModelViewTruncatesSelectedUnplacedReasonInFooter(t *testing.T) {
	m := watchAllModel{
		width:  90,
		height: 12,
		cursor: 0,
		unplacedJobs: []*db.Job{
			{
				ID:               189,
				Status:           db.StatusQueued,
				WorkingDir:       "/tmp/project-gamma",
				Description:      "benchmark",
				GPUClass:         "L40s",
				PlacementReasons: []string{"no local host matched gpu-class=L40s, gpu-mem>=20GB", "2 hosts: no L40s GPU", "another long reason"},
			},
		},
	}

	out := stripANSI(m.View())
	if !strings.Contains(out, "#189 unplaced:") {
		t.Fatalf("footer missing unplaced prefix, got:\n%s", out)
	}
	if !strings.Contains(out, "...") {
		t.Fatalf("footer should truncate detail, got:\n%s", out)
	}
	if strings.Contains(out, "another long reason") {
		t.Fatalf("footer should omit overflowing detail, got:\n%s", out)
	}
}
