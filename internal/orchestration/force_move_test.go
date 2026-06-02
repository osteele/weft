package orchestration

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestSnapshotForcedSourcesAdmitsRunningOnPrem(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Host: "cool30", Status: db.StatusRunning, StartTime: 1000},
		{ID: 2, Host: "cool100", Status: db.StatusStarting, StartTime: 2000},
		{ID: 3, Host: "studio", Status: db.StatusPaused, StartTime: 3000},
	}
	got := SnapshotForcedSources(jobs)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (running, starting, paused all admitted)", len(got))
	}
	for i, want := range []int64{1, 2, 3} {
		if got[i].JobID != want {
			t.Errorf("got[%d].JobID = %d, want %d", i, got[i].JobID, want)
		}
	}
	if got[0].StartTime != 1000 {
		t.Errorf("StartTime not preserved: got %d, want 1000", got[0].StartTime)
	}
}

func TestSnapshotForcedSourcesSkipsQueued(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Host: "cool30", Status: db.StatusQueued, StartTime: 1000},
		{ID: 2, Host: "cool30", Status: db.StatusCompleted, StartTime: 2000},
		{ID: 3, Host: "cool30", Status: db.StatusFailed, StartTime: 3000},
	}
	got := SnapshotForcedSources(jobs)
	if len(got) != 0 {
		t.Errorf("expected no snapshots (no running jobs), got %d", len(got))
	}
}

func TestSnapshotForcedSourcesSkipsCloudSources(t *testing.T) {
	// Cloud sources are handled by LaunchCampaign's R2 cancel marker, not by
	// SSH kill — so SnapshotForcedSources should drop them. HasInventoryHost
	// returns false when the host is empty or a cloud-instance handle.
	rentalLaunchID := int64(42)
	jobs := []*db.Job{
		{ID: 1, Host: "", Status: db.StatusRunning, LaunchID: &rentalLaunchID, StartTime: 1000},
	}
	got := SnapshotForcedSources(jobs)
	if len(got) != 0 {
		t.Errorf("cloud source admitted: got %d snapshots, want 0", len(got))
	}
}

func TestIsMoveAdmissibleStatus(t *testing.T) {
	cases := []struct {
		status string
		force  bool
		want   bool
	}{
		{db.StatusQueued, false, true},
		{db.StatusQueued, true, true},
		{db.StatusPendingPlacement, false, true},
		{db.StatusRunning, false, false},
		{db.StatusRunning, true, true},
		{db.StatusStarting, false, false},
		{db.StatusStarting, true, true},
		{db.StatusPaused, false, false},
		{db.StatusPaused, true, true},
		{db.StatusCompleted, false, false},
		{db.StatusCompleted, true, false},
		{db.StatusFailed, true, false},
		{db.StatusKilled, true, false},
	}
	for _, c := range cases {
		got := isMoveAdmissibleStatus(c.status, c.force)
		if got != c.want {
			t.Errorf("isMoveAdmissibleStatus(%q, force=%v) = %v, want %v", c.status, c.force, got, c.want)
		}
	}
}
