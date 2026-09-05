package ops

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventoryqueue"
	"github.com/osteele/weft/internal/opsqueue"
)

type fakeInventoryQueueStore struct {
	objects map[string][]byte
}

func (s *fakeInventoryQueueStore) GetObject(_ context.Context, key string) ([]byte, error) {
	return s.objects[key], nil
}

func (s *fakeInventoryQueueStore) PutObject(_ context.Context, key string, body io.Reader, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	s.objects[key] = data
	return nil
}

func TestAppendQueueCommandUsesR2ForConfiguredHost(t *testing.T) {
	originalLoad := loadQueueConfig
	originalStore := newInventoryQueueStore
	originalSSH := appendQueueCommandSSH
	t.Cleanup(func() {
		loadQueueConfig = originalLoad
		newInventoryQueueStore = originalStore
		appendQueueCommandSSH = originalSSH
	})
	loadQueueConfig = func() (*config.Config, error) {
		return &config.Config{Hosts: map[string]config.HostConfig{
			"studio": {QueueTransport: queueTransportR2Pull},
		}}, nil
	}
	store := &fakeInventoryQueueStore{objects: map[string][]byte{}}
	newInventoryQueueStore = func() (inventoryQueueStore, error) { return store, nil }
	appendQueueCommandSSH = func(string, opsqueue.QueueCommand, opsqueue.AppendCommandOptions) error {
		t.Fatal("R2-pull transport attempted SSH")
		return nil
	}

	if err := appendQueueCommand("studio", opsqueue.NewCancelCommand(42), opsqueue.AppendCommandOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(store.objects) != 1 {
		t.Fatalf("published %d objects, want 1", len(store.objects))
	}
	for key, data := range store.objects {
		const prefix = "inventory/v1/hosts/studio/inbox/"
		if len(key) < len(prefix) || key[:len(prefix)] != prefix {
			t.Fatalf("unexpected request key %q", key)
		}
		var request inventoryqueue.Request
		if err := json.Unmarshal(data, &request); err != nil {
			t.Fatal(err)
		}
		if request.Host != "studio" || request.Command.Op != opsqueue.OpCancel || request.Command.JobID != 42 {
			t.Fatalf("unexpected request: %+v", request)
		}
	}
}

func TestFetchR2RunnerStateRejectsStaleSnapshots(t *testing.T) {
	originalStore := newInventoryQueueStore
	t.Cleanup(func() { newInventoryQueueStore = originalStore })
	key, _ := inventoryqueue.StateKey("studio")
	encoded, _ := json.Marshal(inventoryqueue.State{
		Version: inventoryqueue.Version, Host: "studio",
		UpdatedAt: time.Now().Add(-inventoryStateMaxAge - time.Second),
	})
	store := &fakeInventoryQueueStore{objects: map[string][]byte{key: encoded}}
	newInventoryQueueStore = func() (inventoryQueueStore, error) { return store, nil }
	if _, err := fetchR2RunnerState("studio"); err == nil {
		t.Fatal("stale R2 runner state was accepted")
	}
}

func TestFetchQueueBatchStatusMapsFreshR2RunnerState(t *testing.T) {
	originalLoad := loadQueueConfig
	originalStore := newInventoryQueueStore
	t.Cleanup(func() {
		loadQueueConfig = originalLoad
		newInventoryQueueStore = originalStore
	})
	loadQueueConfig = func() (*config.Config, error) {
		return &config.Config{Hosts: map[string]config.HostConfig{
			"studio": {QueueTransport: queueTransportR2Pull},
		}}, nil
	}
	key, _ := inventoryqueue.StateKey("studio")
	encoded, _ := json.Marshal(inventoryqueue.State{
		Version: inventoryqueue.Version, Host: "studio", UpdatedAt: time.Now(),
		Runner: opsqueue.RunnerState{
			Pending: []int64{41},
			Running: map[string]opsqueue.RunnerJobState{
				"42": {RunID: 7, StartedAt: 1234, GPUDevices: []string{"0", "1"}},
			},
			Finished: map[string]opsqueue.RunnerFinishedState{
				"43": {ExitCode: 3, FinishedAt: 5678},
			},
		},
	})
	store := &fakeInventoryQueueStore{objects: map[string][]byte{key: encoded}}
	newInventoryQueueStore = func() (inventoryQueueStore, error) { return store, nil }

	statuses, err := fetchQueueBatchStatus("studio", []int64{41, 42, 43}, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if statuses[41].State != queueStateQueued || !statuses[41].FromR2 {
		t.Fatalf("unexpected pending status: %+v", statuses[41])
	}
	if statuses[42].State != queueStateRunning || statuses[42].RunID != 7 || statuses[42].Mtime != 1234 || statuses[42].GPUDevices != "0,1" {
		t.Fatalf("unexpected running status: %+v", statuses[42])
	}
	if statuses[43].ExitCode == nil || *statuses[43].ExitCode != 3 || statuses[43].Mtime != 5678 {
		t.Fatalf("unexpected finished status: %+v", statuses[43])
	}
}

func TestSyncMissingR2CompletionsChecksDispatchedAndRunningJobs(t *testing.T) {
	originalSync := syncJobStatusFromR2ForBatch
	t.Cleanup(func() { syncJobStatusFromR2ForBatch = originalSync })
	var checked []int64
	syncJobStatusFromR2ForBatch = func(_ *sql.DB, job *db.Job) (SyncResult, error) {
		checked = append(checked, job.ID)
		return SyncResult{Updated: true}, nil
	}
	jobs := []*db.Job{
		{ID: 41, LastSyncedStatus: db.StatusQueued},  // unobserved: aged out of the runner window
		{ID: 42, LastSyncedStatus: db.StatusRunning}, // observed running: may already have finished
		{ID: 43, LastSyncedStatus: db.StatusQueued},  // observed queued: not started, nothing to close
		{ID: 44}, // never dispatched: no speculative lookup
	}
	statuses := map[int64]queueBatchStatus{
		42: {State: queueStateRunning},
		43: {State: queueStateQueued},
	}
	if got := syncMissingR2Completions(nil, jobs, statuses); got != 2 {
		t.Fatalf("syncMissingR2Completions() = %d, want 2", got)
	}
	if len(checked) != 2 || checked[0] != 41 || checked[1] != 42 {
		t.Fatalf("completion checks = %v, want [41 42]", checked)
	}
}

// A runner that still reports an attempt as running is reporting what it last
// noticed, not whether the worker exited. The worker's completion record is
// positive evidence that it did, and skipping the check for observed-running
// jobs is how completed work held its queue slot for hours and head-blocked
// every job behind it.
func TestARunningJobStillConsultsItsCompletionRecord(t *testing.T) {
	originalSync := syncJobStatusFromR2ForBatch
	t.Cleanup(func() { syncJobStatusFromR2ForBatch = originalSync })
	checked := false
	syncJobStatusFromR2ForBatch = func(_ *sql.DB, job *db.Job) (SyncResult, error) {
		checked = true
		return SyncResult{Updated: true}, nil
	}
	jobs := []*db.Job{{ID: 6782, LastSyncedStatus: db.StatusRunning}}
	statuses := map[int64]queueBatchStatus{6782: {State: queueStateRunning}}

	if got := syncMissingR2Completions(nil, jobs, statuses); got != 1 {
		t.Fatalf("a running job with a completion record was not closed: got %d", got)
	}
	if !checked {
		t.Fatal("the completion record of a job the runner calls running was never consulted")
	}
}
