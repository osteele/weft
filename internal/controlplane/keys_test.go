package controlplane

import "testing"

func TestExtractRunID(t *testing.T) {
	tests := []struct {
		key  string
		want int64
	}{
		{key: "jobs/441/.complete", want: 0},
		{key: "jobs/441/runs/123/.complete", want: 123},
		{key: "jobs/441/runs/not-a-number/.complete", want: 0},
	}

	for _, tt := range tests {
		if got := ExtractRunID(tt.key); got != tt.want {
			t.Errorf("ExtractRunID(%q) = %d, want %d", tt.key, got, tt.want)
		}
	}
}

func TestInstanceObservabilityKeys(t *testing.T) {
	if got := InstanceAgentDied(42); got != "instance/42/agent-died.json" {
		t.Fatalf("InstanceAgentDied = %q", got)
	}
}

func TestBlackboardKeys(t *testing.T) {
	if got := BlackboardPrefix(); got != "blackboard/v1/" {
		t.Fatalf("BlackboardPrefix = %q", got)
	}
	if got := BlackboardAutopilotState(); got != "blackboard/v1/state/autopilot.json" {
		t.Fatalf("BlackboardAutopilotState = %q", got)
	}
	if got := BlackboardJobSpec(42); got != "blackboard/v1/jobs/42/spec.json" {
		t.Fatalf("BlackboardJobSpec = %q", got)
	}
	if got := BlackboardJobClaim(42); got != "blackboard/v1/jobs/42/claim.json" {
		t.Fatalf("BlackboardJobClaim = %q", got)
	}
	if got := BlackboardJobAssignment(42); got != "blackboard/v1/jobs/42/assignment.json" {
		t.Fatalf("BlackboardJobAssignment = %q", got)
	}
	if got := BlackboardAgentHeartbeat("agent-1"); got != "blackboard/v1/agents/agent-1/heartbeat.json" {
		t.Fatalf("BlackboardAgentHeartbeat = %q", got)
	}
	if got := BlackboardEvent("evt-1"); got != "blackboard/v1/events/evt-1.json" {
		t.Fatalf("BlackboardEvent = %q", got)
	}
}
