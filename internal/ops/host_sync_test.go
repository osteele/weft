package ops

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	srcsync "github.com/osteele/weft/internal/sync"
)

// TestMaterializeCloudNeeds_RefreshesStaleMetadata verifies that
// materializeCloudNeeds re-reads the job's metadata from the DB before
// deciding whether to skip staging. ListUnsyncedQueuedJobs can read a job row
// before the submitter has written job_attempts.job_metadata (the two writes
// are not atomic), leaving CloudNeeds empty on the in-memory struct. Without
// the fresh re-read the job would be dispatched with missing inputs — the
// wj1088 incident.
func TestMaterializeCloudNeeds_RefreshesStaleMetadata(t *testing.T) {
	database := db.SetupTestDB(t)

	// Producer job for the artifact.
	producerID, err := db.RecordQueued(database, "test-host", "/tmp", "produce", "producer")
	if err != nil {
		t.Fatalf("record producer: %v", err)
	}

	// Consumer job. Simulate the race: metadata is persisted in the DB, but
	// the in-memory Job struct we pass in (as ListUnsyncedQueuedJobs would
	// have produced moments earlier) has Metadata=nil.
	consumerID, err := db.RecordQueued(database, "test-host", "/tmp", "consume", "consumer")
	if err != nil {
		t.Fatalf("record consumer: %v", err)
	}
	meta := &db.JobMetadata{
		Dependencies: &db.JobDependencyMetadata{
			CloudNeeds: []string{fmt.Sprintf("output/artifact.bin:%d", producerID)},
		},
	}
	if err := db.SetJobMetadata(database, consumerID, meta); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}

	stale, err := db.GetJobByID(database, consumerID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	// Forcibly clear the metadata to mimic the stale read.
	stale.Metadata = nil

	// getR2Client returns an unconfigured client, which causes
	// materializeCloudNeeds to return an R2-not-configured error *only if*
	// it gets past the metadata check. If the fresh re-read fails to populate
	// metadata, the function returns nil silently — exactly the bug we're
	// guarding against.
	getR2 := func() (*r2.Client, error) { return &r2.Client{}, nil }
	err = materializeCloudNeeds(database, stale, 5*time.Second, getR2)
	if err == nil {
		t.Fatal("expected materializeCloudNeeds to progress past the metadata check (stale metadata should have been refreshed)")
	}
	if !strings.Contains(err.Error(), "R2 is not configured") {
		t.Fatalf("expected 'R2 is not configured' error after refresh, got: %v", err)
	}
}

func TestEnsureQueuedJobsOnRemote(t *testing.T) {
	database := db.SetupTestDB(t)

	// Create a queued job with no LastSyncedStatus (simulates job recorded while host offline)
	_, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "unsynced job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Mock sync so rsync doesn't actually run
	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return nil
	}))

	// Mock SSH so AppendJobToQueue and ResolveBackend succeed
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "", 0
	})

	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 1 {
		t.Errorf("expected 1 job ensured, got %d", ensured)
	}
	if !contacted {
		t.Error("expected contacted=true")
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsJobOnSyncFailure(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "sync-fail job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Mock sync to fail
	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return fmt.Errorf("rsync timeout")
	}))

	// Mock SSH — should never be called since sync fails first
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		t.Error("SSH should not be called when sync fails")
		return "", "", 0
	})

	ensured, _, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, slog.Default())
	if err == nil {
		t.Fatal("expected ensureQueuedJobsOnRemote to surface the sync failure")
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (sync failed), got %d", ensured)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("job %d source sync failed", jobID)) {
		t.Fatalf("error = %q, want job-specific source sync failure", err)
	}

	// Job should still be unsynced in the database
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.LastSyncedStatus != "" {
		t.Errorf("expected LastSyncedStatus empty (unsynced), got %q", job.LastSyncedStatus)
	}
}

func TestSyncHost_SurfacesQueueDispatchFailure(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "sync-fail job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	t.Cleanup(srcsync.SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return fmt.Errorf("rsync timeout")
	}))

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		return "", "", 0
	})

	result, err := SyncHost(database, "test-host", HostSyncOptions{Timeout: time.Second}, nil)
	if err != nil {
		t.Fatalf("SyncHost: %v", err)
	}
	if result.QueueDispatchError == "" {
		t.Fatal("expected QueueDispatchError to be populated")
	}
	if !strings.Contains(result.QueueDispatchError, fmt.Sprintf("job %d source sync failed", jobID)) {
		t.Fatalf("QueueDispatchError = %q, want job-specific source sync failure", result.QueueDispatchError)
	}
	if result.Updated != 0 {
		t.Fatalf("Updated = %d, want 0", result.Updated)
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsAlreadySynced(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "synced job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Mark as already synced
	if err := db.UpdateLastSyncedStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("update last synced status: %v", err)
	}

	// No SSH mock needed — should skip without making SSH calls
	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (already synced), got %d", ensured)
	}
	if contacted {
		t.Error("expected contacted=false for already synced")
	}
}

func TestEnsureQueuedJobsOnRemote_SkipsPendingStatus(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "pending job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	// Set a pending status (e.g., pending cancel)
	cancelStatus := db.StatusCanceled
	if err := db.SetPendingStatus(database, jobID, cancelStatus); err != nil {
		t.Fatalf("set pending status: %v", err)
	}

	ensured, contacted, err := ensureQueuedJobsOnRemote(database, "test-host", 5*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("ensureQueuedJobsOnRemote: %v", err)
	}
	if ensured != 0 {
		t.Errorf("expected 0 jobs ensured (has pending status), got %d", ensured)
	}
	if contacted {
		t.Error("expected contacted=false for pending status")
	}
}

func TestProcessDeferredQueueOps_RemoveQueued(t *testing.T) {
	database := db.SetupTestDB(t)

	jobID, err := db.RecordQueued(database, "test-host", "/tmp", "echo hello", "queued job")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.AddDeferredOperation(database, "test-host", db.OpRemoveQueued, jobID, ""); err != nil {
		t.Fatalf("add deferred op: %v", err)
	}
	if err := db.MoveQueuedJobToUnplaced(database, jobID); err != nil {
		t.Fatalf("MoveQueuedJobToUnplaced: %v", err)
	}

	callCount := 0
	mockSSHFunc(t, func(host, command string) (string, string, int) {
		callCount++
		return "", "", 0
	})

	result, err := ProcessDeferredQueueOps(database, "test-host", 5*time.Second)
	if err != nil {
		t.Fatalf("ProcessDeferredQueueOps: %v", err)
	}
	if !result.HostContacted {
		t.Error("expected HostContacted to be true")
	}
	if callCount == 0 {
		t.Error("expected remote cleanup SSH command")
	}
	pending, err := db.HasPendingOperation(database, jobID, db.OpRemoveQueued)
	if err != nil {
		t.Fatalf("HasPendingOperation: %v", err)
	}
	if pending {
		t.Error("expected remove_queued deferred op to be cleared")
	}
}

func TestEnsureHFInputsAvailable_DownloadsMissingHFAsset(t *testing.T) {
	database := db.SetupTestDB(t)
	scanCount := 0

	mockSSHFunc(t, func(host, command string) (string, string, int) {
		if host != "test-host" {
			t.Fatalf("unexpected host %q", host)
		}
		switch {
		case strings.Contains(command, "df -Pk"):
			return "20971520\n", "", 0
		case strings.Contains(command, "$_hfdl download --repo-type model"):
			return "", "", 0
		case strings.Contains(command, "du -sb"), strings.Contains(command, "ls -1d"):
			scanCount++
			if scanCount >= 2 {
				return "2048\t/home/test/.cache/huggingface/hub/models--bert-base-uncased\n", "", 0
			}
			return "", "", 0
		default:
			return "", "", 0
		}
	})

	if err := ensureHFInputsAvailable(database, "test-host", []string{"hf:bert-base-uncased"}, 5*time.Second); err != nil {
		t.Fatalf("ensureHFInputsAvailable: %v", err)
	}

	entries, err := dataloc.FindAssetHosts(database, dataloc.DataAsset{Kind: dataloc.AssetHFModel, ID: "bert-base-uncased"})
	if err != nil {
		t.Fatalf("FindAssetHosts: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Host != "test-host" {
		t.Fatalf("host = %q, want test-host", entries[0].Host)
	}
}

func TestHFInputStageTimeout(t *testing.T) {
	if got := hfInputStageTimeout(0); got != minHFInputStageTimeout {
		t.Fatalf("hfInputStageTimeout(0) = %s, want %s", got, minHFInputStageTimeout)
	}
	if got := hfInputStageTimeout(30 * time.Second); got != minHFInputStageTimeout {
		t.Fatalf("hfInputStageTimeout(30s) = %s, want %s", got, minHFInputStageTimeout)
	}
	longTimeout := 15 * time.Minute
	if got := hfInputStageTimeout(longTimeout); got != longTimeout {
		t.Fatalf("hfInputStageTimeout(15m) = %s, want %s", got, longTimeout)
	}
}
