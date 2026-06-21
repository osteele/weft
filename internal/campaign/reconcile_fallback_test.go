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
