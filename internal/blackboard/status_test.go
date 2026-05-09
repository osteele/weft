package blackboard

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/r2"
)

type fakeStore struct {
	objects map[string]string
}

func (s fakeStore) GetObject(_ context.Context, key string) ([]byte, error) {
	body, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("missing")
	}
	return []byte(body), nil
}

func (s fakeStore) ListObjects(_ context.Context, prefix string) ([]r2.ObjectInfo, error) {
	var out []r2.ObjectInfo
	for key := range s.objects {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			out = append(out, r2.ObjectInfo{Key: key})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func TestFetchStatusCountsBlackboardObjects(t *testing.T) {
	now := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
	store := fakeStore{objects: map[string]string{
		controlplane.BlackboardAutopilotState():        `{"version":"v1","state":"idle","exported_at":"2026-05-09T12:00:00Z"}`,
		controlplane.BlackboardJobSpec(1):              `{"version":"v1","job_id":1,"command":"echo hi","published_at":"2026-05-09T12:00:00Z"}`,
		controlplane.BlackboardJobClaim(1):             `{"version":"v1","job_id":1,"agent_id":"a","claim_kind":"initial","created_at":"2026-05-09T11:59:00Z","renewed_at":"2026-05-09T11:59:00Z","expires_at":"2026-05-09T12:01:00Z"}`,
		controlplane.BlackboardJobClaim(2):             `{"version":"v1","job_id":2,"agent_id":"b","claim_kind":"initial","created_at":"2026-05-09T11:00:00Z","renewed_at":"2026-05-09T11:00:00Z","expires_at":"2026-05-09T11:01:00Z"}`,
		controlplane.BlackboardJobAssignment(1):        `{"version":"v1","job_id":1,"agent_id":"a","accepted":true,"assigned_at":"2026-05-09T12:00:00Z"}`,
		controlplane.BlackboardAgentHeartbeat("agent"): `{"version":"v1","agent_id":"agent","heartbeat_at":"2026-05-09T12:00:00Z"}`,
		controlplane.BlackboardEvent("evt"):            `{"version":"v1","event_id":"evt","kind":"claim","created_at":"2026-05-09T12:00:00Z"}`,
	}}

	status, err := FetchStatus(context.Background(), store, now)
	if err != nil {
		t.Fatalf("FetchStatus: %v", err)
	}
	if status.Autopilot == nil || status.Autopilot.State != "idle" {
		t.Fatalf("Autopilot = %#v, want idle", status.Autopilot)
	}
	if status.Specs != 1 || status.Claims != 2 || status.ActiveClaims != 1 || status.ExpiredClaims != 1 ||
		status.Assignments != 1 || status.AgentHeartbeats != 1 || status.Events != 1 {
		t.Fatalf("status counts = %+v", status)
	}
}

func TestJobIDFromKey(t *testing.T) {
	got := jobIDFromKey("blackboard/v1/jobs/42/claim.json")
	if got != 42 {
		t.Fatalf("jobIDFromKey = %d, want 42", got)
	}
}
