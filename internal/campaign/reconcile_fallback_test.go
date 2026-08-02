package campaign

import (
	"context"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// Regression: when batch ListAllInstances fails for a provider and the
// per-instance ShowInstance fallback returns ErrInstanceNotFound, the
// reconciler must NOT enter the provider_dead path. Previously the
// fallback collapsed ErrInstanceNotFound into providerErr=nil + inst=nil,
// which let a single transient list-incomplete response start the
// dead-confirm timer (and 30s later mark a still-booting instance dead).
//
// The batch path treats "missing from list" as providerErr-set (skip this
// pass); the fallback path should be symmetric.
func TestReconcileLaunches_FallbackInstanceNotFound_DoesNotMarkDead(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "99999"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		// Batch listing fails entirely → reconciler falls back to ShowInstance.
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return nil, cloud.ErrInstanceNotFound // any error trips the fallback
		},
		// Fallback returns ErrInstanceNotFound (e.g. vastai's list endpoint
		// briefly omits the still-booting instance).
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			return nil, cloud.ErrInstanceNotFound
		},
	}

	// Zero hysteresis so the test runs synchronously: with the old bug, this
	// pass would mark the instance dead. With the fix, providerErr stays
	// non-nil so the provider_dead path is skipped.
	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	if _, err := r.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get launch: %v", err)
	}
	if ci.Status == db.LaunchStatusFailed {
		t.Fatalf("instance was marked failed despite ErrInstanceNotFound being non-authoritative; want still running")
	}
	if ci.Status != db.LaunchStatusRunning {
		t.Errorf("instance status = %q, want %q", ci.Status, db.LaunchStatusRunning)
	}
}

// Regression: a `launching` row has no provider ID until CreateInstance
// returns and SetLaunchProviderID runs — a window that can span minutes of
// create attempts and offer searches. A concurrent reconcile pass sees the
// row with nothing to poll (nil instance, nil error) and must leave it
// alone. Previously isProviderTerminalWithPolicy(nil…) read never-polled as
// dead and rule 7 reaped launches mid-create (the 2026-05-15 16-orphan
// incident: 16 rows failed with "provider returned no instance", no
// provider ID, no launched_at).
func TestReconcileLaunches_LaunchingWithoutProviderID_NotReaped(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusLaunching,
		Provider: "vastai",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{}, nil // batch succeeds; row has no ID to look up
		},
		ShowInstanceFunc: func(id string) (*cloud.Instance, error) {
			// t.Errorf, not Fatalf: this runs on a reconciler goroutine.
			t.Errorf("ShowInstance must not be called for a launch with no provider ID (got %q)", id)
			return nil, cloud.ErrInstanceNotFound
		},
	}

	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1, // with the old bug a single pass marks it dead
	}
	if _, err := r.ReconcileLaunches(context.Background(), database, []cloud.Client{mockClient}, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get launch: %v", err)
	}
	if ci.Status != db.LaunchStatusLaunching {
		t.Fatalf("instance status = %q, want still %q (create in flight)", ci.Status, db.LaunchStatusLaunching)
	}
}

// Regression: a launch whose provider has no configured client in this
// session (partial credentials) is never polled; it must not be declared
// provider-dead by a reconcile pass that simply cannot see it.
func TestReconcileLaunches_MissingProviderClient_NotReaped(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "runpod",
		GPUSpec:  "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "pod-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	// Only a vastai client is configured; the runpod launch cannot be polled.
	vastClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{}, nil
		},
	}

	r := &Reconciler{
		firstDeadAt:        make(map[int64]time.Time),
		lastProviderStatus: make(map[int64]string),
		deadConfirmTime:    -1,
	}
	if _, err := r.ReconcileLaunches(context.Background(), database, []cloud.Client{vastClient}, nil); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get launch: %v", err)
	}
	if ci.Status != db.LaunchStatusRunning {
		t.Fatalf("instance status = %q, want still %q (provider not pollable this session)", ci.Status, db.LaunchStatusRunning)
	}
}
