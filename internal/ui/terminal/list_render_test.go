package terminal

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestFormatJobListStatus_PendingPlacement(t *testing.T) {
	launchID := int64(42)
	cases := []struct {
		name string
		job  *db.Job
		want string
	}{
		{
			// No LaunchID, no Host: the scheduler has no target for the job.
			// The selected-job footer already labels this "unplaced" via
			// TargetKind — the status column must agree.
			name: "no launch and no host is unplaced",
			job:  &db.Job{ID: 1, Status: db.StatusPendingPlacement},
			want: "unplaced",
		},
		{
			// LaunchID set: rental instance launch in progress.
			name: "with launch is launching",
			job:  &db.Job{ID: 2, Status: db.StatusPendingPlacement, LaunchID: &launchID},
			want: "launching",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatJobListStatus(tc.job); got != tc.want {
				t.Errorf("formatJobListStatus(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}
