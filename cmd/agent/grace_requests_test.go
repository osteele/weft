package main

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
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

	payloads := []controlplane.GraceJobsRequest{
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

	jobs, err := drainGraceJobRequests("test-bucket", instanceID, nil)
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

func TestDrainGraceJobRequestsAppliesSourcesBeforeReturningJobs(t *testing.T) {
	instanceID := int64(56)
	prefix := controlplane.GraceJobsPrefix(instanceID)
	objects := map[string]string{}
	acks := map[string]controlplane.GraceCommandAck{}

	remoteDir := filepath.Join(t.TempDir(), "workspace", "project")
	payload := controlplane.GraceJobsRequest{
		Jobs: []cloud.AgentJob{{ID: 99, Command: "python train.py"}},
		Sources: []controlplane.SourceUpdate{{
			RemoteDir: remoteDir,
			R2Key:     "sources/test.tar.gz",
		}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	objects[path.Join(prefix, "req-a.json")] = string(data)

	prevCacheDir := sourceCacheDir
	sourceCacheDir = filepath.Join(t.TempDir(), "source-cache")
	t.Cleanup(func() { sourceCacheDir = prevCacheDir })

	binDir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	rclonePath := filepath.Join(binDir, "rclone")
	rcloneScript := `#!/bin/sh
if [ "$1" != "copyto" ]; then
  echo "unexpected rclone command: $*" >&2
  exit 1
fi
dest="$3"
mkdir -p "$(dirname "$dest")"
printf 'stub' > "$dest"
`
	if err := os.WriteFile(rclonePath, []byte(rcloneScript), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	tarPath := filepath.Join(binDir, "tar")
	tarScript := `#!/bin/sh
if [ "$1" != "xzf" ] || [ "$3" != "-C" ]; then
  echo "unexpected tar command: $*" >&2
  exit 1
fi
dir="$4"
mkdir -p "$dir"
printf 'ok' > "$dir/source-applied.txt"
`
	if err := os.WriteFile(tarPath, []byte(tarScript), 0o755); err != nil {
		t.Fatalf("write fake tar: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	prevList, prevGet, prevPut, prevDelete := graceR2List, graceR2Get, graceR2Put, graceR2Delete
	t.Cleanup(func() {
		graceR2List, graceR2Get, graceR2Put, graceR2Delete = prevList, prevGet, prevPut, prevDelete
	})

	graceR2List = func(_ string, gotPrefix string) ([]string, error) {
		if gotPrefix != prefix {
			t.Fatalf("prefix = %q, want %q", gotPrefix, prefix)
		}
		return []string{"req-a.json"}, nil
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
	graceR2Delete = func(_ string, _ string) error { return nil }

	jobs, err := drainGraceJobRequests("test-bucket", instanceID, nil)
	if err != nil {
		t.Fatalf("drainGraceJobRequests: %v", err)
	}
	if len(jobs) != 1 || jobs[0].ID != 99 {
		t.Fatalf("jobs = %+v, want one job id=99", jobs)
	}
	marker := filepath.Join(remoteDir, "source-applied.txt")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("expected source marker %s: %v", marker, err)
	}
	if len(acks) != 1 {
		t.Fatalf("acks = %v, want one", acks)
	}
	for _, ack := range acks {
		if !ack.Accepted {
			t.Fatalf("ack = %+v, want accepted", ack)
		}
	}
}

func TestDrainGraceJobRequestsRejectsInvalidSourceUpdate(t *testing.T) {
	instanceID := int64(57)
	prefix := controlplane.GraceJobsPrefix(instanceID)
	objects := map[string]string{}
	acks := map[string]controlplane.GraceCommandAck{}

	payload := controlplane.GraceJobsRequest{
		Jobs: []cloud.AgentJob{{ID: 100, Command: "python train.py"}},
		Sources: []controlplane.SourceUpdate{{
			RemoteDir: "",
			R2Key:     "sources/test.tar.gz",
		}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	objects[path.Join(prefix, "req-a.json")] = string(data)

	prevList, prevGet, prevPut, prevDelete := graceR2List, graceR2Get, graceR2Put, graceR2Delete
	t.Cleanup(func() {
		graceR2List, graceR2Get, graceR2Put, graceR2Delete = prevList, prevGet, prevPut, prevDelete
	})

	graceR2List = func(_ string, _ string) ([]string, error) { return []string{"req-a.json"}, nil }
	graceR2Get = func(_ string, key string) (string, error) { return objects[key], nil }
	graceR2Put = func(_ string, key, content string) error {
		var ack controlplane.GraceCommandAck
		if err := json.Unmarshal([]byte(content), &ack); err != nil {
			t.Fatalf("Unmarshal ack: %v", err)
		}
		acks[key] = ack
		return nil
	}
	graceR2Delete = func(_ string, _ string) error { return nil }

	jobs, err := drainGraceJobRequests("test-bucket", instanceID, nil)
	if err != nil {
		t.Fatalf("drainGraceJobRequests: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("jobs = %+v, want none", jobs)
	}
	if len(acks) != 1 {
		t.Fatalf("acks = %v, want one", acks)
	}
	for _, ack := range acks {
		if ack.Accepted {
			t.Fatalf("ack = %+v, want rejected", ack)
		}
	}
}

func TestDrainGraceCancelAttemptRequestsAggregatesIDs(t *testing.T) {
	instanceID := int64(99)
	prefix := controlplane.GraceCancelAttemptsPrefix(instanceID)

	payloads := []controlplane.GraceCancelAttemptsRequest{
		{AttemptIDs: []int64{1001, 1002}},
		{AttemptIDs: []int64{1003}},
	}
	objects := map[string]string{}
	for i, payload := range payloads {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		objects[path.Join(prefix, []string{"req-a", "req-b"}[i]+".json")] = string(data)
	}

	prevList, prevGet, prevPut, prevDelete := graceR2List, graceR2Get, graceR2Put, graceR2Delete
	t.Cleanup(func() {
		graceR2List, graceR2Get, graceR2Put, graceR2Delete = prevList, prevGet, prevPut, prevDelete
	})
	graceR2List = func(_ string, _ string) ([]string, error) {
		return []string{"req-a.json", "req-b.json"}, nil
	}
	graceR2Get = func(_ string, key string) (string, error) { return objects[key], nil }
	graceR2Put = func(_ string, _, _ string) error { return nil }
	graceR2Delete = func(_ string, _ string) error { return nil }

	got, err := drainGraceCancelAttemptRequests("test-bucket", instanceID)
	if err != nil {
		t.Fatalf("drainGraceCancelAttemptRequests: %v", err)
	}
	for _, want := range []int64{1001, 1002, 1003} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing canceled attempt id %d in %v", want, got)
		}
	}
}
