package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/opsqueue"
)

func TestBuildHostAgentStatusRowsClassifiesFreshnessAndSkew(t *testing.T) {
	// The stale-observation bound (hostAgentRuntimeFreshness) tracks the
	// daemon's quiet-period sync cadence, so a host is only stale once its
	// observation is genuinely old — an hour here, versus seconds for the
	// current hosts.
	now := time.Unix(10_000, 0)
	desired := "desired123456"
	hosts := []inventory.HostSpec{{Name: "current"}, {Name: "old"}, {Name: "stale-cache"}, {Name: "unseen"}}
	observations := []db.HostAgentState{
		{
			Host: "current", DeployedVersion: desired, DeployedObservedAt: 9800,
			RunningVersion: desired, QueueProtocolVersion: opsqueue.QueueProtocolVersion, RunningObservedAt: 9990,
		},
		{
			Host: "old", DeployedVersion: desired, DeployedObservedAt: 9800,
			RunningVersion: "old123456789", QueueProtocolVersion: opsqueue.QueueProtocolVersion, RunningObservedAt: 9990,
		},
		{
			Host: "stale-cache", DeployedVersion: desired, DeployedObservedAt: 9800,
			RunningVersion: "old123456789", QueueProtocolVersion: 0, RunningObservedAt: 6400,
		},
	}

	rows := buildHostAgentStatusRows(hosts, observations, func(string, string) (string, error) {
		return desired, nil
	}, now)
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

// A weft process that cannot hash the agent source falls back to a revision
// identity, which disagrees with the source hash of the very tree it names.
// Reporting that as staleness accuses a current host of running an old build.
func TestClassifyHostAgentStatusRevisionFallbackCannotJudgeSourceBuild(t *testing.T) {
	now := time.Unix(1_000, 0)
	for _, tc := range []struct {
		name                        string
		deployed, running           string
		wantStatus, wantReasonMatch string
	}{
		{
			name: "running is a source hash", deployed: "vcs-78ce3e6cd135", running: "ebcd26823c30",
			wantStatus: "unknown", wantReasonMatch: "revision fallback",
		},
		{
			name: "deployed is a source hash", deployed: "ebcd26823c30", running: "vcs-78ce3e6cd135",
			wantStatus: "unknown", wantReasonMatch: "revision fallback",
		},
		{
			name: "every identity is a revision", deployed: "vcs-78ce3e6cd135", running: "vcs-78ce3e6cd135",
			wantStatus: "current", wantReasonMatch: "agree",
		},
		{
			name: "revision identities genuinely differ", deployed: "vcs-78ce3e6cd135", running: "vcs-0008db9dc5f2",
			wantStatus: "stale", wantReasonMatch: "running agent differs",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, reason := classifyHostAgentStatus(hostAgentStatusRow{
				DesiredVersion: "vcs-78ce3e6cd135", DeployedVersion: tc.deployed, RunningVersion: tc.running,
				QueueProtocolVersion: opsqueue.QueueProtocolVersion,
				RunningObservedAt:    990,
			}, now)
			if status != tc.wantStatus || !strings.Contains(reason, tc.wantReasonMatch) {
				t.Fatalf("status=%q reason=%q, want %q containing %q", status, reason, tc.wantStatus, tc.wantReasonMatch)
			}
		})
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
	if err := writeHostAgentStatusJSON(&output, rows, now); err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if document["schema_version"] != float64(hostAgentStatusSchemaVersion) {
		t.Fatalf("schema_version = %#v", document["schema_version"])
	}
	if _, ok := document["desired_version"]; ok {
		t.Fatalf("document-level desired_version must not represent heterogeneous targets: %#v", document)
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

func TestBuildHostAgentStatusRowsResolvesDesiredVersionPerHost(t *testing.T) {
	now := time.Unix(1_000, 0)
	hosts := []inventory.HostSpec{
		{Name: "amd", OS: "linux", Arch: "amd64"},
		{Name: "arm", OS: "linux", Arch: "arm64"},
	}
	observations := []db.HostAgentState{
		{
			Host: "amd", DeployedVersion: "amd-version", DeployedObservedAt: 900,
			RunningVersion: "amd-version", QueueProtocolVersion: opsqueue.QueueProtocolVersion, RunningObservedAt: 990,
		},
		{
			Host: "arm", DeployedVersion: "arm-version", DeployedObservedAt: 900,
			RunningVersion: "arm-version", QueueProtocolVersion: opsqueue.QueueProtocolVersion, RunningObservedAt: 990,
		},
	}
	rows := buildHostAgentStatusRows(hosts, observations, func(goos, goarch string) (string, error) {
		if goos+"-"+goarch == "linux-arm64" {
			return "arm-version", nil
		}
		return "amd-version", nil
	}, now)

	if rows[0].DesiredVersion != "amd-version" || rows[0].Status != "current" {
		t.Fatalf("amd row = %#v", rows[0])
	}
	if rows[1].DesiredVersion != "arm-version" || rows[1].Status != "current" {
		t.Fatalf("arm row = %#v", rows[1])
	}
}

func TestBuildHostAgentStatusRowsResolvesEachTargetOnce(t *testing.T) {
	hosts := []inventory.HostSpec{
		{Name: "first", OS: "linux", Arch: "amd64"},
		{Name: "second", OS: "linux", Arch: "amd64"},
	}
	calls := 0
	rows := buildHostAgentStatusRows(hosts, nil, func(_, _ string) (string, error) {
		calls++
		return "desired", nil
	}, time.Unix(1_000, 0))

	if calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", calls)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestBuildHostAgentStatusRowsIsolatesDesiredVersionFailure(t *testing.T) {
	now := time.Unix(1_000, 0)
	hosts := []inventory.HostSpec{
		{Name: "unresolved", OS: "linux", Arch: "arm64"},
		{Name: "current", OS: "linux", Arch: "amd64"},
	}
	observations := []db.HostAgentState{
		{
			Host: "unresolved", RunningVersion: "old",
			QueueProtocolVersion: opsqueue.QueueProtocolVersion, RunningObservedAt: 990,
		},
		{
			Host: "current", DeployedVersion: "desired", DeployedObservedAt: 900,
			RunningVersion: "desired", QueueProtocolVersion: opsqueue.QueueProtocolVersion, RunningObservedAt: 990,
		},
	}
	rows := buildHostAgentStatusRows(hosts, observations, func(_, goarch string) (string, error) {
		if goarch == "arm64" {
			return "", errors.New("identity unavailable")
		}
		return "desired", nil
	}, now)

	if rows[0].Status != "unknown" || !strings.Contains(rows[0].Reason, "identity unavailable") {
		t.Fatalf("unresolved row = %#v", rows[0])
	}
	if rows[1].Status != "current" {
		t.Fatalf("current row = %#v", rows[1])
	}
}
