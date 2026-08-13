package narrate

import (
	"encoding/json"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/jobview"
)

func TestJobViewActivityContract(t *testing.T) {
	pending := db.StatusQueued
	memGB := 80
	job := &db.Job{
		ID:                   42,
		Host:                 "cool30",
		WorkingDir:           "/work/augur",
		Command:              "python train.py --epochs 10",
		Description:          "training run",
		Project:              "augur",
		Status:               db.StatusRunning,
		PendingStatus:        &pending,
		StartTime:            1_000,
		QueuedAt:             900,
		GPU:                  "0,1",
		GPUClass:             "h100",
		GPUMemGB:             &memGB,
		Tags:                 []string{"benchmark"},
		GeneratedDescription: "ignored generated description",
		Metadata: &db.JobMetadata{Source: &db.JobSourceMetadata{
			Hash: "identity-hash",
			Pin:  &db.JobSourcePinMetadata{Hash: "pinned-hash"},
			Roots: []db.JobSourceRootMetadata{{
				LocalPath:     "/work/augur",
				MountBasename: "augur",
				Hash:          "root-hash",
				VCS: &db.JobSourceVCSMetadata{
					Type: "jj", Revision: "abc123", ChangeID: "change-id", Dirty: true,
				},
			}},
		}},
	}

	view := jobToView(nil, job, nil, jobview.PlacementStatus{
		Bucket:    jobview.BucketQueued,
		DisplayAt: 950,
	})
	if view.Status != db.StatusRunning || view.EffectiveStatus != db.StatusQueued {
		t.Fatalf("stored/effective status = %q/%q", view.Status, view.EffectiveStatus)
	}
	if view.StartTime != 1_000 || view.PlacementAt != 950 || view.StateSince != 950 {
		t.Fatalf("timing fields = start:%d placement:%d state:%d", view.StartTime, view.PlacementAt, view.StateSince)
	}
	if view.Description != "training run" || view.CommandFull != job.Command {
		t.Fatalf("description/command = %q/%q", view.Description, view.CommandFull)
	}
	if view.GPU != "0,1" || view.GPUClass != "h100" || view.GPUMemGB == nil || *view.GPUMemGB != 80 {
		t.Fatalf("GPU fields = %+v", view)
	}
	if view.Source == nil || view.Source.WorkingDir != "/work/augur" || view.Source.PinnedSnapshotHash != "pinned-hash" {
		t.Fatalf("source = %+v", view.Source)
	}
	if len(view.Source.Roots) != 1 || view.Source.Roots[0].VCS == nil || view.Source.Roots[0].VCS.ChangeID != "change-id" {
		t.Fatalf("source roots = %+v", view.Source.Roots)
	}

	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode view fields: %v", err)
	}
	if _, exists := fields["duration"]; exists {
		t.Fatalf("activity contract must not publish a preformatted duration: %s", raw)
	}
}

func TestJobStateSinceUsesOnlyMatchingSemanticTimestamp(t *testing.T) {
	end := int64(2_000)
	job := &db.Job{StartTime: 1_000, QueuedAt: 900, EndTime: &end}
	tests := []struct {
		name        string
		bucket      jobview.Bucket
		placementAt int64
		want        int64
	}{
		{name: "running uses execution start", bucket: jobview.BucketRunning, placementAt: 950, want: 1_000},
		{name: "placement uses placement timestamp", bucket: jobview.BucketLaunching, placementAt: 950, want: 950},
		{name: "queued falls back to queue epoch", bucket: jobview.BucketQueued, want: 900},
		{name: "terminal uses end", bucket: jobview.BucketCompletions, want: 2_000},
		{name: "paused remains unknown", bucket: jobview.BucketPaused, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jobStateSince(job, tt.bucket, tt.placementAt); got != tt.want {
				t.Fatalf("jobStateSince = %d, want %d", got, tt.want)
			}
		})
	}
}
