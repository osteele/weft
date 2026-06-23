package hostsync

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
)

func TestWorkerNoJobsNoSyncTime(t *testing.T) {
	database := db.SetupTestDB(t)
	testHost := "test-host-no-jobs"

	_, _ = database.Exec(`DELETE FROM host_syncs WHERE name = ?`, testHost)

	worker := New(database, nil, nil, nil)
	worker.Start()
	defer worker.Stop()

	worker.Request(Request{Host: testHost, Rate: RateRunning, Priority: true})

	time.Sleep(1 * time.Second)

	times, err := db.LoadHostSyncTimes(database)
	if err != nil {
		t.Fatalf("LoadHostSyncTimes: %v", err)
	}
	if _, ok := times[testHost]; ok {
		t.Errorf("sync time should not be recorded when there are no jobs to sync")
	}
}

func TestGetHostSyncRateUsesEffectiveStatus(t *testing.T) {
	queuedRunning := db.StatusRunning
	jobs := []*db.Job{
		{Status: db.StatusQueued, PendingStatus: &queuedRunning, Host: "test-host"},
	}

	if got := GetHostSyncRate(jobs); got != RateRunning {
		t.Fatalf("GetHostSyncRate() = %v, want %v", got, RateRunning)
	}
}

func TestGetHostSyncModePromotesUnsyncedQueuedInventoryJob(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Host: "test-host", Status: db.StatusQueued},
	}

	if got := GetHostSyncMode(jobs); got != ops.SyncModeFull {
		t.Fatalf("GetHostSyncMode() = %v, want %v", got, ops.SyncModeFull)
	}
}

func TestGetHostSyncModeKeepsStatusForAlreadyDispatchedQueuedJob(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Host: "test-host", Status: db.StatusQueued, LastSyncedStatus: db.StatusQueued, QueuedAt: time.Now().Unix()},
	}

	if got := GetHostSyncMode(jobs); got != ops.SyncModeStatus {
		t.Fatalf("GetHostSyncMode() = %v, want %v", got, ops.SyncModeStatus)
	}
}

func TestGetHostSyncModePromotesStaleDispatchedQueuedJob(t *testing.T) {
	jobs := []*db.Job{
		{
			ID:               1,
			Host:             "test-host",
			Status:           db.StatusQueued,
			LastSyncedStatus: db.StatusQueued,
			QueuedAt:         time.Now().Add(-queuedDispatchReconcileGrace - time.Second).Unix(),
		},
	}

	if got := GetHostSyncMode(jobs); got != ops.SyncModeFull {
		t.Fatalf("GetHostSyncMode() = %v, want %v", got, ops.SyncModeFull)
	}
}

func TestGetHostSyncModePromotesDispatchedQueuedJobWithoutQueuedAt(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Host: "test-host", Status: db.StatusQueued, LastSyncedStatus: db.StatusQueued},
	}

	if got := GetHostSyncMode(jobs); got != ops.SyncModeFull {
		t.Fatalf("GetHostSyncMode() = %v, want %v", got, ops.SyncModeFull)
	}
}

func TestGetHostSyncModePromotesPendingQueuedInventoryJob(t *testing.T) {
	pending := db.StatusQueued
	jobs := []*db.Job{
		{ID: 1, Host: "test-host", Status: db.StatusQueued, LastSyncedStatus: db.StatusQueued, PendingStatus: &pending},
	}

	if got := GetHostSyncMode(jobs); got != ops.SyncModeFull {
		t.Fatalf("GetHostSyncMode() = %v, want %v", got, ops.SyncModeFull)
	}
}

func TestGetHostSyncModeIgnoresJobsThatCannotBeDispatched(t *testing.T) {
	pending := db.StatusCanceled
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusQueued},
		{ID: 2, Host: "vastai:123", Status: db.StatusQueued},
		{ID: 3, Host: "test-host", Status: db.StatusQueued, PendingStatus: &pending},
	}

	if got := GetHostSyncMode(jobs); got != ops.SyncModeStatus {
		t.Fatalf("GetHostSyncMode() = %v, want %v", got, ops.SyncModeStatus)
	}
}

func TestBuildWarning(t *testing.T) {
	tests := []struct {
		name   string
		result Result
		want   string
	}{
		{
			name: "agent deploy failure",
			result: Result{
				QueueRunnerError: "agent deploy failed: version mismatch",
			},
			want: "agent deploy failed: version mismatch",
		},
		{
			name: "queue runner start failure",
			result: Result{
				QueueRunnerError: "queue runner start failed: tmux missing",
			},
			want: "queue runner start failed: tmux missing",
		},
		{
			name: "queue dispatch failure",
			result: Result{
				QueueDispatchError: "job 289 source sync failed",
			},
			want: "queue dispatch failed: job 289 source sync failed",
		},
		{
			name: "multiple failures collapse into one warning",
			result: Result{
				QueueRunnerError:   "agent deploy failed: version mismatch",
				QueueDispatchError: "job 289 source sync failed",
			},
			want: "agent deploy failed: version mismatch; queue dispatch failed: job 289 source sync failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildWarning(toHostSyncResult(tt.result))
			if got != tt.want {
				t.Fatalf("BuildWarning() = %q, want %q", got, tt.want)
			}
		})
	}
}

func toHostSyncResult(result Result) ops.HostSyncResult {
	return ops.HostSyncResult{
		QueueDispatchError: result.QueueDispatchError,
		QueueRunnerError:   result.QueueRunnerError,
	}
}
