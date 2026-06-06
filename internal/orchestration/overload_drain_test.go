package orchestration

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestJobPinnedToCurrentHost(t *testing.T) {
	tests := []struct {
		name string
		job  *db.Job
		want bool
	}{
		{
			name: "explicit host override pins matching host",
			job: &db.Job{
				Host:                 "cool30",
				PlacementMeta:        &db.PlacementMeta{},
				CLIResourceOverrides: &db.CLIResourceOverrides{Host: "cool30"},
			},
			want: true,
		},
		{
			name: "auto placed job is movable",
			job:  &db.Job{Host: "cool30", PlacementMeta: &db.PlacementMeta{}},
			want: false,
		},
		{
			name: "legacy host assignment is pinned",
			job:  &db.Job{Host: "cool30"},
			want: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := jobPinnedToCurrentHost(tc.job); got != tc.want {
				t.Fatalf("jobPinnedToCurrentHost = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestOverloadRunningEvacuationAllowed(t *testing.T) {
	now := time.Unix(10_000, 0)
	pred := 3600.0
	tests := []struct {
		name string
		job  *db.Job
		want bool
	}{
		{
			name: "early auto placed running job",
			job: &db.Job{
				StartTime:     now.Add(-5 * time.Minute).Unix(),
				PlacementMeta: &db.PlacementMeta{PredictedDurationS: &pred},
			},
			want: true,
		},
		{
			name: "too much elapsed time",
			job: &db.Job{
				StartTime:     now.Add(-20 * time.Minute).Unix(),
				PlacementMeta: &db.PlacementMeta{PredictedDurationS: &pred},
			},
			want: false,
		},
		{
			name: "no placement telemetry",
			job:  &db.Job{StartTime: now.Add(-time.Minute).Unix()},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := overloadRunningEvacuationAllowed(tc.job, now); got != tc.want {
				t.Fatalf("overloadRunningEvacuationAllowed = %t, want %t", got, tc.want)
			}
		})
	}
}
