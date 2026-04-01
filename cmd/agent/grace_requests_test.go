package main

import (
	"encoding/json"
	"path"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
)

func TestDrainGraceJobRequestsAggregatesQueuedPayloads(t *testing.T) {
	instanceID := int64(55)
	prefix := controlplane.GraceJobsPrefix(instanceID)
	objects := map[string]string{}
	acks := map[string]controlplane.GraceCommandAck{}
	var deleted []string

	payloads := []graceJobsPayload{
		{Jobs: []cloud.AgentJob{{ID: 1, Command: "echo 1"}}},
		{Jobs: []cloud.AgentJob{{ID: 2, Command: "echo 2"}}},
	}
	for i, payload := range payloads {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("Marshal payload %d: %v", i, err)
		}
		requestID := []string{"req-a", "req-b"}[i]
		objects[path.Join(prefix, requestID+".json")] = string(data)
	}

	prevList, prevGet, prevPut, prevDelete := graceR2List, graceR2Get, graceR2Put, graceR2Delete
	t.Cleanup(func() {
		graceR2List, graceR2Get, graceR2Put, graceR2Delete = prevList, prevGet, prevPut, prevDelete
	})

	graceR2List = func(_ string, gotPrefix string) ([]string, error) {
		if gotPrefix != prefix {
			t.Fatalf("prefix = %q, want %q", gotPrefix, prefix)
		}
		return []string{"req-a.json", "req-b.json"}, nil
	}
	graceR2Get = func(_ string, key string) (string, error) {
		return objects[key], nil
	}
	graceR2Put = func(_ string, key, content string) error {
		var ack controlplane.GraceCommandAck
		if err := json.Unmarshal([]byte(content), &ack); err != nil {
			t.Fatalf("Unmarshal ack: %v", err)
		}
		acks[key] = ack
		return nil
	}
	graceR2Delete = func(_ string, key string) error {
		deleted = append(deleted, key)
		return nil
	}

	jobs, err := drainGraceJobRequests("test-bucket", instanceID)
	if err != nil {
		t.Fatalf("drainGraceJobRequests: %v", err)
	}
	if len(jobs) != 2 || jobs[0].ID != 1 || jobs[1].ID != 2 {
		t.Fatalf("jobs = %+v, want IDs [1 2]", jobs)
	}
	if len(deleted) != 2 {
		t.Fatalf("deleted = %v, want both requests removed", deleted)
	}
	if len(acks) != 2 {
		t.Fatalf("acks = %v, want one ack per request", acks)
	}
	for _, ack := range acks {
		if !ack.Accepted || ack.Kind != controlplane.GraceCommandJobs {
			t.Fatalf("ack = %+v, want accepted jobs ack", ack)
		}
	}
}
