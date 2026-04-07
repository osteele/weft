// Tests derived from specs/status-sync.allium — ThreeWayMerge rule.
// Tests the pure decision logic of the Reconcile function for each of the
// 6 cases identified in the spec, plus conflict resolution.
package ops

import (
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

// mockSSHForReconcile sets up SSH mocks that succeed for all operations.
func mockSSHForReconcile(t *testing.T) {
	t.Helper()
	mockSSHFunc(t, func(host, cmd string) (string, string, int) {
		// Accept all SSH commands — we're testing the merge logic, not SSH.
		return "", "", 0
	})
}

func TestSpec_ThreeWayMerge_Case1_NoChange(t *testing.T) {
	// Case 1: No local intent and remote unchanged from base → no-op.
	database := db.SetupTestDB(t)
	mockSSHForReconcile(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "echo hello", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	// Set last_synced_status to "running" (matches current status).
	if err := db.UpdateStatusAndLastSynced(database, jobID, db.StatusRunning); err != nil {
		t.Fatalf("set last_synced: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	result, err := Reconcile(database, job, db.StatusRunning, ReconcileOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Action != "none" {
		t.Errorf("action = %q, want 'none'", result.Action)
	}
}

func TestSpec_ThreeWayMerge_Case2_RemoteChanged(t *testing.T) {
	// Case 2: Remote changed, no local intent → accept remote (pure sync).
	database := db.SetupTestDB(t)
	mockSSHForReconcile(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "echo hello", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := db.UpdateStatusAndLastSynced(database, jobID, db.StatusRunning); err != nil {
		t.Fatalf("set last_synced: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	// Remote reports completed while we still think running.
	result, err := Reconcile(database, job, db.StatusCompleted, ReconcileOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Action != "update_db" {
		t.Errorf("action = %q, want 'update_db'", result.Action)
	}
	if result.NewStatus != db.StatusCompleted {
		t.Errorf("new_status = %q, want 'completed'", result.NewStatus)
	}

	// Verify DB state.
	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusCompleted {
		t.Errorf("job.Status = %q, want 'completed'", updated.Status)
	}
	if updated.LastSyncedStatus != db.StatusCompleted {
		t.Errorf("last_synced = %q, want 'completed'", updated.LastSyncedStatus)
	}
}

func TestSpec_ThreeWayMerge_Case4_Convergence(t *testing.T) {
	// Case 4: Both changed to same state → convergence, clear pending.
	database := db.SetupTestDB(t)
	mockSSHForReconcile(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "echo hello", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := db.UpdateStatusAndLastSynced(database, jobID, db.StatusRunning); err != nil {
		t.Fatalf("set last_synced: %v", err)
	}
	// User wants to kill.
	if err := db.SetPendingStatus(database, jobID, db.StatusKilled); err != nil {
		t.Fatalf("set pending: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	// Remote also reports killed — convergence.
	result, err := Reconcile(database, job, db.StatusKilled, ReconcileOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Action != "update_db" {
		t.Errorf("action = %q, want 'update_db'", result.Action)
	}

	// Verify pending cleared.
	updated, _ := db.GetJobByID(database, jobID)
	if updated.PendingStatus != nil {
		t.Errorf("pending_status should be nil after convergence, got %v", updated.PendingStatus)
	}
	if updated.Status != db.StatusKilled {
		t.Errorf("status = %q, want 'killed'", updated.Status)
	}
}

func TestSpec_ThreeWayMerge_Case6_ConflictTerminalRemoteWins(t *testing.T) {
	// Case 6: Conflict — remote reached terminal state, local wanted something else.
	// Terminal remote wins (unless local wants requeue).
	database := db.SetupTestDB(t)
	mockSSHForReconcile(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "echo hello", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := db.UpdateStatusAndLastSynced(database, jobID, db.StatusRunning); err != nil {
		t.Fatalf("set last_synced: %v", err)
	}
	// User wanted to pause.
	if err := db.SetPendingStatus(database, jobID, db.StatusPaused); err != nil {
		t.Fatalf("set pending: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	// Remote says completed — terminal wins over local pause intent.
	result, err := Reconcile(database, job, db.StatusCompleted, ReconcileOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.Conflict {
		t.Error("expected conflict flag to be set")
	}
	if !strings.Contains(result.Resolution, "accepted terminal remote state") {
		t.Errorf("resolution = %q, expected to mention 'accepted terminal remote state'", result.Resolution)
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.Status != db.StatusCompleted {
		t.Errorf("status = %q, want 'completed' (terminal remote wins)", updated.Status)
	}
	if updated.PendingStatus != nil {
		t.Error("pending_status should be cleared after conflict resolution")
	}
}

func TestSpec_ThreeWayMerge_Case6_RequeueOverridesTerminal(t *testing.T) {
	// Case 6 exception: local wants requeue → overrides stale terminal state.
	database := db.SetupTestDB(t)
	mockSSHForReconcile(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "echo hello", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := db.UpdateStatusAndLastSynced(database, jobID, db.StatusRunning); err != nil {
		t.Fatalf("set last_synced: %v", err)
	}
	// User wants requeue.
	if err := db.SetPendingStatus(database, jobID, db.StatusQueued); err != nil {
		t.Fatalf("set pending: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	// Remote says failed — but requeue intent should override.
	result, err := Reconcile(database, job, db.StatusFailed, ReconcileOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.Conflict {
		t.Error("expected conflict flag to be set")
	}
	if !strings.Contains(result.Resolution, "requeued over stale terminal state") {
		t.Errorf("resolution = %q, expected requeue override", result.Resolution)
	}
}

func TestSpec_PendingStatusClearedAfterApply(t *testing.T) {
	// Invariant: pending_status must be cleared after successful application.
	// Test cases 2 and 4 already verify this. This tests case 3 (pure ops).
	database := db.SetupTestDB(t)
	mockSSHForReconcile(t)

	jobID, err := db.RecordJobStarting(database, "test-host", "/tmp", "echo hello", "test")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.MarkRunningByID(database, jobID); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	if err := db.UpdateStatusAndLastSynced(database, jobID, db.StatusRunning); err != nil {
		t.Fatalf("set last_synced: %v", err)
	}
	// User wants to cancel.
	if err := db.SetPendingStatus(database, jobID, db.StatusCanceled); err != nil {
		t.Fatalf("set pending: %v", err)
	}

	job, _ := db.GetJobByID(database, jobID)

	// Remote unchanged (still running) — apply cancel to remote.
	result, err := Reconcile(database, job, db.StatusRunning, ReconcileOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if result.Action == "none" {
		t.Error("expected an action, got 'none'")
	}

	updated, _ := db.GetJobByID(database, jobID)
	if updated.PendingStatus != nil {
		t.Errorf("pending_status should be nil after apply, got %v", *updated.PendingStatus)
	}
}
