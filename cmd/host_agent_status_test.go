package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/opsqueue"
)

func TestBuildHostAgentStatusRowsClassifiesFreshnessAndSkew(t *testing.T) {
	now := time.Unix(1_000, 0)
	desired := "desired123456"
	hosts := []inventory.HostSpec{{Name: "current"}, {Name: "old"}, {Name: "stale-cache"}, {Name: "unseen"}}
	observations := []db.HostAgentState{
		{
			Host: "current", DeployedVersion: desired, DeployedObservedAt: 800,
			RunningVersion: desired, QueueProtocolVersion: opsqueue.QueueProtocolVersion, RunningObservedAt: 990,
		},
		{
			Host: "old", DeployedVersion: desired, DeployedObservedAt: 800,
			RunningVersion: "old123456789", QueueProtocolVersion: opsqueue.QueueProtocolVersion, RunningObservedAt: 990,
		},
		{
			Host: "stale-cache", DeployedVersion: desired, DeployedObservedAt: 800,
			RunningVersion: "old123456789", QueueProtocolVersion: 0, RunningObservedAt: 700,
		},
	}

	rows := buildHostAgentStatusRows(hosts, observations, desired, now)
	if len(rows) != 4 {
		t.Fatalf("rows = %#v", rows)
	}
	want := []string{"current", "stale", "stale-observation", "unknown"}
	for i := range rows {
		if rows[i].Status != want[i] {
			t.Errorf("%s status = %q, want %q (%s)", rows[i].Host, rows[i].Status, want[i], rows[i].Reason)
		}
	}
	if !strings.Contains(rows[1].Reason, "running agent differs") {
		t.Errorf("stale reason = %q", rows[1].Reason)
	}
}

func TestClassifyHostAgentStatusRequiresCurrentProtocol(t *testing.T) {
	now := time.Unix(1_000, 0)
	status, reason := classifyHostAgentStatus(hostAgentStatusRow{
		DesiredVersion: "same", DeployedVersion: "same", RunningVersion: "same",
		QueueProtocolVersion: opsqueue.QueueProtocolVersion - 1,
		RunningObservedAt:    990,
	}, now)
	if status != "stale" || !strings.Contains(reason, "older than required") {
		t.Fatalf("status=%q reason=%q", status, reason)
	}
}

func TestClassifyHostAgentStatusFreshRunningSkewOutweighsMissingDeploymentObservation(t *testing.T) {
	status, reason := classifyHostAgentStatus(hostAgentStatusRow{
		DesiredVersion: "desired", RunningVersion: "old",
		QueueProtocolVersion: opsqueue.QueueProtocolVersion,
		RunningObservedAt:    990,
	}, time.Unix(1_000, 0))
	if status != "stale" || !strings.Contains(reason, "running agent differs") {
		t.Fatalf("status=%q reason=%q", status, reason)
	}
}

func TestWriteHostAgentStatusJSONPublishesVersionedEvidence(t *testing.T) {
	now := time.Unix(1_000, 0)
	rows := []hostAgentStatusRow{{
		Host: "host-alpha", Status: "stale", Reason: "running agent differs",
		DesiredVersion: "desired", DeployedVersion: "desired", DeployedObservedAt: 900,
		RunningVersion: "old", QueueProtocolVersion: 1, RunningObservedAt: 990,
	}}
	var output bytes.Buffer
	if err := writeHostAgentStatusJSON(&output, rows, "desired", now); err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if document["schema_version"] != float64(hostAgentStatusSchemaVersion) {
		t.Fatalf("schema_version = %#v", document["schema_version"])
	}
	hosts, ok := document["hosts"].([]any)
	if !ok || len(hosts) != 1 {
		t.Fatalf("hosts = %#v", document["hosts"])
	}
	host, ok := hosts[0].(map[string]any)
	if !ok || host["deployed_observed_at"] != float64(900) || host["running_observed_at"] != float64(990) {
		t.Fatalf("host evidence = %#v", hosts[0])
	}
}
