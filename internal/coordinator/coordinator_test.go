package coordinator

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/intent"
	"github.com/osteele/weft/internal/ssh"
)

func TestIdempotency(t *testing.T) {
	// Set up SSH mock to avoid real SSH calls
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		return "", "", nil
	})
	defer cleanup()

	dir := t.TempDir()
	config := DefaultConfig()
	config.IntentDir = filepath.Join(dir, "intents")
	config.ArchiveDir = filepath.Join(dir, "intents", "archive")

	c := New(nil, config) // nil db since we're just testing idempotency

	// Mark an intent as processed
	c.processed["test-intent-1"] = true

	// Write an intent file with the same ID
	i := &intent.Intent{
		Timestamp: time.Now(),
		Op:        "place",
		IntentID:  "test-intent-1",
		Source:    "test",
		Job: intent.IntentJob{
			ID:  1,
			Cmd: "echo hello",
			Dir: "/tmp",
		},
	}
	data, _ := json.Marshal(i)

	os.MkdirAll(config.IntentDir, 0755)
	os.MkdirAll(config.ArchiveDir, 0755)
	path := filepath.Join(config.IntentDir, i.Filename())
	os.WriteFile(path, data, 0644)

	// Process the intent — it should be skipped (idempotent)
	c.handleIntentFile(path)

	// The intent should have been archived
	if _, err := os.Stat(filepath.Join(config.ArchiveDir, i.Filename())); os.IsNotExist(err) {
		t.Error("duplicate intent was not archived")
	}
}

func TestHostStateTransitions(t *testing.T) {
	// Set up SSH mock
	hostOnline := true
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		if hostOnline {
			return "", "", nil
		}
		return "", "connection timed out", &sshError{}
	})
	defer cleanup()

	c := New(nil, DefaultConfig())

	// Initially probe — should be online
	if !c.probeHost("testhost") {
		t.Error("expected host to be online")
	}

	states := c.HostStates()
	if !states["testhost"].Online {
		t.Error("expected testhost to be online in state map")
	}

	// Host goes offline
	hostOnline = false
	if c.probeHost("testhost") {
		t.Error("expected host to be offline")
	}

	states = c.HostStates()
	if states["testhost"].Online {
		t.Error("expected testhost to be offline in state map")
	}

	// Host comes back online
	hostOnline = true
	if !c.probeHost("testhost") {
		t.Error("expected host to be online again")
	}
}

func TestRetryQueue(t *testing.T) {
	q := newRetryQueue()

	i1 := &intent.Intent{IntentID: "a"}
	i2 := &intent.Intent{IntentID: "b"}
	i3 := &intent.Intent{IntentID: "c"}

	q.Add(i1, "host1")
	q.Add(i2, "host2")
	q.Add(i3, "host1")

	if q.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", q.Len())
	}

	drained := q.DrainForHost("host1")
	if len(drained) != 2 {
		t.Fatalf("DrainForHost(host1) = %d items, want 2", len(drained))
	}
	if drained[0].intent.IntentID != "a" || drained[1].intent.IntentID != "c" {
		t.Error("unexpected drained intents")
	}

	if q.Len() != 1 {
		t.Fatalf("Len() after drain = %d, want 1", q.Len())
	}

	drained = q.DrainForHost("host2")
	if len(drained) != 1 {
		t.Fatalf("DrainForHost(host2) = %d items, want 1", len(drained))
	}

	if q.Len() != 0 {
		t.Fatalf("Len() after full drain = %d, want 0", q.Len())
	}
}

func TestScanExistingIntents(t *testing.T) {
	dir := t.TempDir()
	intentDir := filepath.Join(dir, "intents")
	os.MkdirAll(intentDir, 0755)

	// Create some intent files and some non-intent files
	os.WriteFile(filepath.Join(intentDir, "1-abc.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(intentDir, "2-def.json"), []byte("{}"), 0644)
	os.WriteFile(filepath.Join(intentDir, ".tmp.XXXXXX"), []byte("{}"), 0644) // should be excluded
	os.MkdirAll(filepath.Join(intentDir, "subdir"), 0755)                     // should be excluded

	paths, err := scanExistingIntents(intentDir)
	if err != nil {
		t.Fatalf("scanExistingIntents: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("got %d paths, want 2", len(paths))
	}
}

func TestScanExistingIntentsNonexistent(t *testing.T) {
	paths, err := scanExistingIntents("/nonexistent/path")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(paths) != 0 {
		t.Fatalf("got %d paths, want 0", len(paths))
	}
}

func TestCoordinatorRunCancellation(t *testing.T) {
	dir := t.TempDir()
	config := DefaultConfig()
	config.IntentDir = filepath.Join(dir, "intents")
	config.ArchiveDir = filepath.Join(dir, "intents", "archive")
	config.PIDFile = filepath.Join(dir, "coordinator.pid")
	config.LogPath = filepath.Join(dir, "coordinator.log")
	config.PollInterval = 100 * time.Millisecond
	config.RetryInterval = 100 * time.Millisecond
	config.SyncInterval = 100 * time.Millisecond

	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		return "", "", nil
	})
	defer cleanup()

	c := New(nil, config)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := c.Run(ctx)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// PID file should be cleaned up
	if _, err := os.Stat(config.PIDFile); !os.IsNotExist(err) {
		t.Error("PID file was not cleaned up")
	}
}

// sshError satisfies the error interface for SSH error mocking.
type sshError struct{}

func (e *sshError) Error() string { return "ssh: connection timed out" }
