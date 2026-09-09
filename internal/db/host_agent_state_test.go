package db

import (
	"testing"
	"time"
)

func TestHostAgentStatePreservesIndependentObservations(t *testing.T) {
	database := SetupTestDB(t)
	deployedAt := time.Unix(100, 0)
	runningAt := time.Unix(200, 0)

	if err := RecordHostAgentDeployment(database, "host-alpha", "deploy-v1", deployedAt); err != nil {
		t.Fatal(err)
	}
	if err := RecordHostAgentRuntime(database, "host-alpha", "running-v1", 1, runningAt); err != nil {
		t.Fatal(err)
	}
	if err := RecordHostAgentDeployment(database, "host-alpha", "deploy-v2", time.Unix(300, 0)); err != nil {
		t.Fatal(err)
	}

	states, err := ListHostAgentStates(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 {
		t.Fatalf("states = %#v, want one", states)
	}
	got := states[0]
	if got.DeployedVersion != "deploy-v2" || got.DeployedObservedAt != 300 {
		t.Errorf("deployed observation = %q at %d", got.DeployedVersion, got.DeployedObservedAt)
	}
	if got.RunningVersion != "running-v1" || got.QueueProtocolVersion != 1 || got.RunningObservedAt != 200 {
		t.Errorf("running observation = %q protocol %d at %d", got.RunningVersion, got.QueueProtocolVersion, got.RunningObservedAt)
	}
}

func TestHostAgentStateRecordsLegacyRuntimeIdentity(t *testing.T) {
	database := SetupTestDB(t)
	if err := RecordHostAgentRuntime(database, "host-alpha", "", 0, time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}

	states, err := ListHostAgentStates(database)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].RunningObservedAt != 200 || states[0].QueueProtocolVersion != 0 {
		t.Fatalf("legacy observation = %#v", states)
	}
}
