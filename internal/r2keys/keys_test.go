package r2keys

import (
	"testing"

	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/dataplane"
)

func TestControlAndDataPlaneJobRunPrefixesStayInLockstep(t *testing.T) {
	jobID := int64(441)
	runID := int64(123)
	if got, want := controlplane.JobPrefix(jobID), dataplane.JobPrefix(jobID); got != want {
		t.Fatalf("JobPrefix mismatch: controlplane=%q dataplane=%q", got, want)
	}
	if got, want := controlplane.JobRunPrefix(jobID, runID), dataplane.JobRunPrefix(jobID, runID); got != want {
		t.Fatalf("JobRunPrefix mismatch: controlplane=%q dataplane=%q", got, want)
	}
	if got, want := controlplane.JobRunPrefix(jobID, 0), dataplane.JobRunPrefix(jobID, 0); got != want {
		t.Fatalf("legacy JobRunPrefix mismatch: controlplane=%q dataplane=%q", got, want)
	}
}

func TestExtractRunID(t *testing.T) {
	tests := []struct {
		key  string
		want int64
	}{
		{"jobs/441/runs/123/.complete", 123},
		{"jobs/441/.complete", 0},
		{"jobs/441/runs/abc/.complete", 0},
		{"", 0},
	}
	for _, tt := range tests {
		got := ExtractRunID(tt.key)
		if got != tt.want {
			t.Errorf("ExtractRunID(%q) = %d, want %d", tt.key, got, tt.want)
		}
	}
}

func TestCoordinatorRelayKeys(t *testing.T) {
	if got := CoordinatorRelayRequest("req-1"); got != "coordinator/v1/inbox/req-1.json" {
		t.Fatalf("CoordinatorRelayRequest = %q", got)
	}
	if got := CoordinatorRelayAck("req-1"); got != "coordinator/v1/acks/req-1.json" {
		t.Fatalf("CoordinatorRelayAck = %q", got)
	}
	if got := CoordinatorRelayInboxPrefix(); got != "coordinator/v1/inbox/" {
		t.Fatalf("CoordinatorRelayInboxPrefix = %q", got)
	}
	if got := CoordinatorRelayAckPrefix(); got != "coordinator/v1/acks/" {
		t.Fatalf("CoordinatorRelayAckPrefix = %q", got)
	}
}

func TestInstanceObservabilityKeys(t *testing.T) {
	if got := InstanceAgentDied(42); got != "instance/42/agent-died.json" {
		t.Fatalf("InstanceAgentDied = %q", got)
	}
}
