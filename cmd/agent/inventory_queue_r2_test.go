package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/inventoryqueue"
	"github.com/osteele/weft/internal/opsqueue"
)

func TestInventoryQueueR2BridgeAppendsAcknowledgesAndPublishesState(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	request := inventoryqueue.Request{
		Version: inventoryqueue.Version, RequestID: "request-1", Host: "studio", CreatedAt: now,
		Command: opsqueue.NewPriorityCommand(42),
	}
	requestData, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	requestKey, _ := inventoryqueue.RequestKey("studio", "request-1")
	objects := map[string]string{requestKey: string(requestData)}
	deleted := map[string]bool{}
	bridge := &inventoryQueueR2Bridge{
		bucket: "bucket", host: "studio",
		commandsFile: filepath.Join(dir, opsqueue.CommandsFileName()),
		stateFile:    filepath.Join(dir, opsqueue.StateFileName()),
		agentVersion: "test-version", now: func() time.Time { return now },
		get:    func(_, key string) (string, error) { return objects[key], nil },
		put:    func(_, key, data string) error { objects[key] = data; return nil },
		list:   func(_, prefix string) ([]string, error) { return []string{"request-1.json"}, nil },
		delete: func(_, key string) error { delete(objects, key); deleted[key] = true; return nil },
	}

	if err := bridge.runOnce(); err != nil {
		t.Fatal(err)
	}
	commands, err := os.ReadFile(bridge.commandsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(commands), `"op":"priority"`) || !strings.Contains(string(commands), `"job_id":42`) {
		t.Fatalf("unexpected command log: %s", commands)
	}
	ackKey, _ := inventoryqueue.AckKey("studio", "request-1")
	if objects[ackKey] == "" {
		t.Fatal("ack was not published")
	}
	if !deleted[requestKey] {
		t.Fatal("request was not deleted after acknowledgement")
	}
	stateKey, _ := inventoryqueue.StateKey("studio")
	var state inventoryqueue.State
	if err := json.Unmarshal([]byte(objects[stateKey]), &state); err != nil {
		t.Fatal(err)
	}
	if state.Host != "studio" || state.AgentVersion != "test-version" || !state.UpdatedAt.Equal(now) {
		t.Fatalf("unexpected published state: %+v", state)
	}
}

func TestInventoryQueueR2BridgeDoesNotAppendAnAcknowledgedRequestAgain(t *testing.T) {
	dir := t.TempDir()
	request := inventoryqueue.Request{
		Version: inventoryqueue.Version, RequestID: "request-1", Host: "studio",
		Command: opsqueue.NewCancelCommand(42),
	}
	requestData, _ := json.Marshal(request)
	requestKey, _ := inventoryqueue.RequestKey("studio", "request-1")
	ackKey, _ := inventoryqueue.AckKey("studio", "request-1")
	ackData, _ := json.Marshal(inventoryqueue.Ack{
		Version: inventoryqueue.Version, RequestID: "request-1", Host: "studio", ProcessedAt: time.Now(),
	})
	objects := map[string]string{requestKey: string(requestData), ackKey: string(ackData)}
	bridge := &inventoryQueueR2Bridge{
		bucket: "bucket", host: "studio",
		commandsFile: filepath.Join(dir, opsqueue.CommandsFileName()),
		stateFile:    filepath.Join(dir, opsqueue.StateFileName()), now: time.Now,
		get:    func(_, key string) (string, error) { return objects[key], nil },
		put:    func(_, key, data string) error { objects[key] = data; return nil },
		list:   func(_, prefix string) ([]string, error) { return []string{"request-1.json"}, nil },
		delete: func(_, key string) error { delete(objects, key); return nil },
	}

	if err := bridge.runOnce(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bridge.commandsFile); !os.IsNotExist(err) {
		t.Fatalf("acknowledged request was appended again: stat error %v", err)
	}
}

func TestInventoryQueueR2BridgeProcessesInboxWhenStatePublishFails(t *testing.T) {
	dir := t.TempDir()
	request := inventoryqueue.Request{
		Version: inventoryqueue.Version, RequestID: "request-1", Host: "studio",
		Command: opsqueue.NewCancelCommand(42),
	}
	requestData, _ := json.Marshal(request)
	requestKey, _ := inventoryqueue.RequestKey("studio", "request-1")
	stateKey, _ := inventoryqueue.StateKey("studio")
	objects := map[string]string{requestKey: string(requestData)}
	bridge := &inventoryQueueR2Bridge{
		bucket: "bucket", host: "studio",
		commandsFile: filepath.Join(dir, opsqueue.CommandsFileName()),
		stateFile:    filepath.Join(dir, opsqueue.StateFileName()), now: time.Now,
		get: func(_, key string) (string, error) { return objects[key], nil },
		put: func(_, key, data string) error {
			if key == stateKey {
				return os.ErrPermission
			}
			objects[key] = data
			return nil
		},
		list:   func(_, prefix string) ([]string, error) { return []string{"request-1.json"}, nil },
		delete: func(_, key string) error { delete(objects, key); return nil },
	}

	if err := bridge.runOnce(); err == nil || !strings.Contains(err.Error(), "publish state") {
		t.Fatalf("runOnce() error = %v, want state publication error", err)
	}
	commands, err := os.ReadFile(bridge.commandsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(commands), `"op":"cancel"`) {
		t.Fatalf("inbox request was not processed after state failure: %s", commands)
	}
}
