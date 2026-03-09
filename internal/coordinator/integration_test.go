package coordinator_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/coordinator"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/intent"
	"github.com/osteele/weft/internal/ssh"
	srcsync "github.com/osteele/weft/internal/sync"
)

// Integration tests for the coordinator daemon.
// These tests require SSH_TEST_HOST to be set.

func getTestHost(t *testing.T) string {
	host := os.Getenv("SSH_TEST_HOST")
	if host == "" {
		t.Skip("SSH_TEST_HOST not set - skipping integration test")
	}
	return host
}

func TestIntegrationWriteAndParseIntent(t *testing.T) {
	host := getTestHost(t)

	i := &intent.Intent{
		Timestamp: time.Now(),
		Op:        "place",
		IntentID:  "integration-test-1",
		Source:    "test",
		Job: intent.IntentJob{
			ID:  999,
			Cmd: "echo integration-test",
			Dir: "/tmp",
		},
	}

	// Write intent to remote host
	err := intent.WriteIntent(host, i)
	if err != nil {
		t.Fatalf("WriteIntent: %v", err)
	}

	// Verify the file arrived
	remotePath := intent.RemoteIntentDir + "/" + i.Filename()
	stdout, _, err := ssh.RunWithTimeout(host, "cat "+remotePath, 5*time.Second)
	if err != nil {
		t.Fatalf("read remote intent: %v", err)
	}

	var parsed intent.Intent
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		t.Fatalf("parse remote intent: %v", err)
	}
	if parsed.IntentID != i.IntentID {
		t.Errorf("IntentID = %q, want %q", parsed.IntentID, i.IntentID)
	}

	// Clean up
	ssh.RunWithTimeout(host, "rm -f "+remotePath, 5*time.Second)
}

func TestIntegrationCoordinatorProcessesIntent(t *testing.T) {
	host := getTestHost(t)

	database := db.SetupTestDB(t)

	dir := t.TempDir()
	config := coordinator.DefaultConfig()
	config.IntentDir = filepath.Join(dir, "intents")
	config.ArchiveDir = filepath.Join(dir, "intents", "archive")
	config.PIDFile = filepath.Join(dir, "coordinator.pid")
	config.LogPath = filepath.Join(dir, "coordinator.log")
	config.PollInterval = 5 * time.Second
	config.RetryInterval = 5 * time.Second
	config.SyncInterval = 60 * time.Second

	// Don't mock SSH — use real SSH for integration test
	c := coordinator.New(database, config)

	// Write an intent file with explicit host constraint (the test host)
	i := &intent.Intent{
		Timestamp: time.Now(),
		Op:        "place",
		IntentID:  "integration-coord-1",
		Source:    "test",
		Job: intent.IntentJob{
			ID:  1000,
			Cmd: "echo coord-integration",
			Dir: "/tmp",
			Constraints: intent.IntentConstraints{
				Host: host,
			},
		},
	}
	data, _ := json.Marshal(i)
	os.MkdirAll(config.IntentDir, 0755)
	os.MkdirAll(config.ArchiveDir, 0755)
	intentPath := filepath.Join(config.IntentDir, i.Filename())
	os.WriteFile(intentPath, data, 0644)

	// Start coordinator with short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Run coordinator briefly — it should process the catch-up intent on startup
	go func() {
		c.Run(ctx)
	}()

	// Wait for coordinator to process the intent
	time.Sleep(3 * time.Second)
	cancel()

	// The intent should have been archived
	if _, err := os.Stat(intentPath); !os.IsNotExist(err) {
		t.Error("intent file was not archived after processing")
	}
	archivePath := filepath.Join(config.ArchiveDir, i.Filename())
	if _, err := os.Stat(archivePath); os.IsNotExist(err) {
		t.Error("intent file was not found in archive directory")
	}

	// Check for outcome file
	outcomePath := filepath.Join(config.ArchiveDir, intent.OutcomeFilename(i.IntentID))
	if _, err := os.Stat(outcomePath); os.IsNotExist(err) {
		t.Error("outcome file was not written")
	}

	// Clean up remote state
	ssh.RunWithTimeout(host, "rm -rf ~/.cache/weft/queue/default.queue ~/.cache/weft/logs/1000-*", 5*time.Second)
}

func TestIntegrationCatchupOnStartup(t *testing.T) {
	// Verify that intents placed before coordinator starts are still processed.
	// This uses a mock SSH runner to avoid real SSH and focus on catch-up logic.
	cleanup := ssh.SetRunner(func(host, command string) (string, string, error) {
		return "", "", nil
	})
	defer cleanup()

	// Mock rsync to prevent real process spawning (which would hang on unreachable hosts)
	syncCleanup := srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return nil
	})
	defer syncCleanup()

	database := db.SetupTestDB(t)

	dir := t.TempDir()
	config := coordinator.DefaultConfig()
	config.IntentDir = filepath.Join(dir, "intents")
	config.ArchiveDir = filepath.Join(dir, "intents", "archive")
	config.PIDFile = filepath.Join(dir, "coordinator.pid")
	config.LogPath = filepath.Join(dir, "coordinator.log")
	config.PollInterval = 100 * time.Millisecond
	config.RetryInterval = 100 * time.Millisecond
	config.SyncInterval = 100 * time.Millisecond

	// Place intent files before coordinator starts
	os.MkdirAll(config.IntentDir, 0755)
	os.MkdirAll(config.ArchiveDir, 0755)

	for idx, id := range []string{"catchup-1", "catchup-2"} {
		i := &intent.Intent{
			Timestamp: time.Now(),
			Op:        "place",
			IntentID:  id,
			Source:    "test",
			Job: intent.IntentJob{
				ID:  int64(2000 + idx),
				Cmd: "echo " + id,
				Dir: "/tmp",
				Constraints: intent.IntentConstraints{
					Host: "host-beta", // use known inventory host
				},
			},
		}
		data, _ := json.Marshal(i)
		os.WriteFile(filepath.Join(config.IntentDir, i.Filename()), data, 0644)
	}

	c := coordinator.New(database, config)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c.Run(ctx)

	// Both intents should have been archived
	remaining, _ := os.ReadDir(config.IntentDir)
	jsonCount := 0
	for _, entry := range remaining {
		if filepath.Ext(entry.Name()) == ".json" {
			jsonCount++
		}
	}
	if jsonCount != 0 {
		t.Errorf("expected 0 intent files remaining, got %d", jsonCount)
	}

	// Both should be in archive
	archived, _ := os.ReadDir(config.ArchiveDir)
	intentCount := 0
	for _, entry := range archived {
		if filepath.Ext(entry.Name()) == ".json" {
			intentCount++
		}
	}
	// 2 intents + 2 outcomes = 4 files
	if intentCount < 2 {
		t.Errorf("expected at least 2 archived files, got %d", intentCount)
	}
}
