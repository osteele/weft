package tui

import (
	"os"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// Integration tests for SyncWorker host sync time recording.
// These tests verify that sync times are recorded on successful host contact.
//
// Run with: SSH_TEST_HOST=user@host go test -v ./internal/tui/... -run "Integration"

func getTestHost(t *testing.T) string {
	host := os.Getenv("SSH_TEST_HOST")
	if host == "" {
		t.Skip("SSH_TEST_HOST not set - skipping integration test")
	}
	if os.Getenv("SSH_AUTH_SOCK") == "" {
		t.Skip("SSH_AUTH_SOCK not set - skipping integration test")
	}
	return host
}

func TestIntegration_SyncWorkerRecordsSyncTimeOnSuccess(t *testing.T) {
	host := getTestHost(t)
	database := db.SetupTestDB(t)

	// Clear any existing sync time for this host
	_, _ = database.Exec(`DELETE FROM host_syncs WHERE name = ?`, host)

	// Create a sync worker
	worker := NewSyncWorker(database, nil, nil)
	worker.Start()
	defer worker.Stop()

	// Create a queued job that will trigger a sync
	_, err := db.RecordQueued(database, host, "/tmp", "echo test", "test job for sync")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	// Trigger sync for this host
	worker.Request(SyncRequest{Host: host, Rate: RateRunning, Priority: true})

	// Wait for sync to complete (with timeout)
	deadline := time.Now().Add(30 * time.Second)
	var syncTime time.Time
	for time.Now().Before(deadline) {
		times, err := db.LoadHostSyncTimes(database)
		if err != nil {
			t.Fatalf("LoadHostSyncTimes: %v", err)
		}
		if st, ok := times[host]; ok {
			syncTime = st
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if syncTime.IsZero() {
		t.Fatalf("Expected sync time to be recorded for reachable host %s", host)
	}

	// Verify sync time is recent (within last minute)
	if time.Since(syncTime) > time.Minute {
		t.Errorf("Sync time %v is too old (expected within last minute)", syncTime)
	}
}

// TestSyncWorkerNoJobsNoSyncTime verifies that when there are no jobs to sync,
// no sync time is recorded (since we didn't actually contact the host).
func TestSyncWorkerNoJobsNoSyncTime(t *testing.T) {
	database := db.SetupTestDB(t)
	testHost := "test-host-no-jobs"

	// Clear any existing sync time
	_, _ = database.Exec(`DELETE FROM host_syncs WHERE name = ?`, testHost)

	// Create a sync worker
	worker := NewSyncWorker(database, nil, nil)
	worker.Start()
	defer worker.Stop()

	// DON'T create any jobs - just trigger a sync request
	worker.Request(SyncRequest{Host: testHost, Rate: RateRunning, Priority: true})

	// Wait for the sync to process
	time.Sleep(1 * time.Second)

	// Verify NO sync time was recorded (no jobs = no host contact)
	times, err := db.LoadHostSyncTimes(database)
	if err != nil {
		t.Fatalf("LoadHostSyncTimes: %v", err)
	}

	if _, ok := times[testHost]; ok {
		t.Errorf("Sync time should NOT be recorded when there are no jobs to sync")
	}
}
