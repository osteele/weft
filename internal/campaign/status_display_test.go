package campaign

import (
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestDisplayInstanceStatus(t *testing.T) {
	tests := []struct {
		name string
		ci   *db.Launch
		want string
	}{
		{name: "running", ci: &db.Launch{Status: db.LaunchStatusRunning}, want: "running"},
		{name: "failed maps to terminated", ci: &db.Launch{Status: db.LaunchStatusFailed}, want: "terminated"},
		{name: "cancelled maps to terminated", ci: &db.Launch{Status: db.LaunchStatusCancelled}, want: "terminated"},
		{name: "grace label", ci: &db.Launch{Status: db.LaunchStatusGrace}, want: "grace period"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DisplayInstanceStatus(tt.ci); got != tt.want {
				t.Fatalf("DisplayInstanceStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDisplayTerminationReason(t *testing.T) {
	tests := []struct {
		name string
		ci   *db.Launch
		want string
	}{
		{name: "none when empty", ci: &db.Launch{}, want: ""},
		{name: "none when completed", ci: &db.Launch{TerminationReason: db.TerminationReasonCompleted}, want: ""},
		{name: "job failure wording", ci: &db.Launch{TerminationReason: db.TerminationReasonJobFailure}, want: "job failed"},
		{name: "disk full", ci: &db.Launch{TerminationReason: db.TerminationReasonDiskFull}, want: "disk full"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DisplayTerminationReason(tt.ci); got != tt.want {
				t.Fatalf("DisplayTerminationReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDisplayInstanceStatusWithReason(t *testing.T) {
	ci := &db.Launch{Status: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonJobFailure}
	if got := DisplayInstanceStatusWithReason(ci); got != "terminated (job failed)" {
		t.Fatalf("DisplayInstanceStatusWithReason() = %q, want %q", got, "terminated (job failed)")
	}
}
