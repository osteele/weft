package ops

import (
	"testing"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
)

type mockQueueRemote struct {
	statusExitCode int
	statusOption   Option[bool]
	current        Option[bool]
	inQueue        Option[bool]
	process        Option[bool]
	quickStatus    quickStatus
	metadata       string
}

func (m mockQueueRemote) StatusFile(host string, jobID int64, timeout time.Duration) (int, Option[bool]) {
	return m.statusExitCode, m.statusOption
}

func (m mockQueueRemote) CurrentJob(host, queueName string, jobID int64, timeout time.Duration) Option[bool] {
	return m.current
}

func (m mockQueueRemote) InQueue(host, queueName string, jobID int64, timeout time.Duration) Option[bool] {
	return m.inQueue
}

func (m mockQueueRemote) ProcessRunning(host string, jobID int64, timeout time.Duration) Option[bool] {
	return m.process
}

func (m mockQueueRemote) QuickStatus(host, queueName string, jobID int64, timeout time.Duration) (quickStatus, error) {
	return m.quickStatus, nil
}

func (m mockQueueRemote) Metadata(host string, jobID int64, timeout time.Duration) (string, error) {
	return m.metadata, nil
}

func TestSyncQueueRunnerJobQueuedToRunning(t *testing.T) {
	database := setupTestDB(t)

	jobID, err := db.RecordQueued(database, "queue-host", "/tmp", "echo queued", "queued job", "default")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	mock := mockQueueRemote{
		statusOption: Some(false),
		current:      Some(true),
		inQueue:      Some(false),
		process:      Some(true),
		metadata:     "start_time=1700000000\n",
	}

	restore := setQueueRemoteClientForTesting(mock)
	defer restore()

	changed, err := SyncQueueRunnerJob(database, job, SyncOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("SyncQueueRunnerJob: %v", err)
	}
	if !changed {
		t.Fatalf("expected change when transitioning queued job to running")
	}

	updated, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get updated job: %v", err)
	}
	if updated.Status != db.StatusRunning {
		t.Fatalf("expected job status running, got %s", updated.Status)
	}
	if updated.StartTime != 1700000000 {
		t.Fatalf("expected start time updated from metadata, got %d", updated.StartTime)
	}
}
