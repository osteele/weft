package main

import (
	"encoding/json"
	"path"
	"sync"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/controlplane"
)

func TestJobRequestPoller_TakeDrainsAcksAndEmpties(t *testing.T) {
	const instanceID = int64(7001)
	prefix := controlplane.GraceJobsPrefix(instanceID)

	var mu sync.Mutex
	objects := map[string]string{}
	acked := map[string]controlplane.GraceCommandAck{}
	payload := controlplane.GraceJobsRequest{Jobs: []cloud.AgentJob{{ID: 42, RunID: 4242, Command: "echo hi"}}}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	objects[path.Join(prefix, "req-mid.json")] = string(data)

	prevList, prevGet, prevPut, prevDelete := graceR2List, graceR2Get, graceR2Put, graceR2Delete
	t.Cleanup(func() {
		graceR2List, graceR2Get, graceR2Put, graceR2Delete = prevList, prevGet, prevPut, prevDelete
	})
	graceR2List = func(_ string, gotPrefix string) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		var names []string
		for key := range objects {
			if path.Dir(key) == path.Clean(gotPrefix) {
				names = append(names, path.Base(key))
			}
		}
		return names, nil
	}
	graceR2Get = func(_ string, key string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return objects[key], nil
	}
	graceR2Put = func(_ string, key, content string) error {
		mu.Lock()
		defer mu.Unlock()
		var ack controlplane.GraceCommandAck
		if err := json.Unmarshal([]byte(content), &ack); err != nil {
			t.Errorf("Unmarshal ack: %v", err)
			return nil
		}
		acked[key] = ack
		return nil
	}
	graceR2Delete = func(_ string, key string) error {
		mu.Lock()
		defer mu.Unlock()
		delete(objects, key)
		return nil
	}

	poller := startJobRequestPoller("test-bucket", instanceID)
	defer poller.Stop()

	jobs := poller.Take(nil)
	if len(jobs) != 1 || jobs[0].ID != 42 {
		t.Fatalf("jobs = %+v, want the one pending job", jobs)
	}
	mu.Lock()
	remaining := len(objects)
	ackCount := len(acked)
	mu.Unlock()
	if remaining != 0 {
		t.Fatalf("requests remaining = %d, want request deleted after drain", remaining)
	}
	if ackCount != 1 {
		t.Fatalf("acks = %d, want 1", ackCount)
	}

	if again := poller.Take(nil); len(again) != 0 {
		t.Fatalf("second Take = %+v, want empty", again)
	}

	poller.Stop()
	poller.Stop() // idempotent
}
