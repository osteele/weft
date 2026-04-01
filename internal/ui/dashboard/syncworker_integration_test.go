//go:build integration
// +build integration

package dashboard

import (
	"os"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func getIntegrationTestHost(t *testing.T) string {
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
	host := getIntegrationTestHost(t)
	database := db.SetupTestDB(t)

	_, _ = database.Exec(`DELETE FROM host_syncs WHERE name = ?`, host)

	worker := NewSyncWorker(database, nil, nil, nil)
	worker.Start()
	defer worker.Stop()

	_, err := db.RecordQueued(database, host, "/tmp", "echo test", "test job for sync")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	worker.Request(SyncRequest{Host: host, Rate: RateRunning, Priority: true})

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
		t.Fatalf("expected sync time to be recorded for reachable host %s", host)
	}
	if time.Since(syncTime) > time.Minute {
		t.Errorf("sync time %v is too old (expected within last minute)", syncTime)
	}
}
