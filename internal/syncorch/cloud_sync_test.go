package syncorch

import (
	"database/sql"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

type panicCloudClient struct{}

func (panicCloudClient) Provider() cloud.Provider { return cloud.ProviderVastai }
func (panicCloudClient) Available() error         { return nil }
func (panicCloudClient) SearchOffers(cloud.OfferConstraints) ([]cloud.Offer, error) {
	panic("cloud client should not be contacted while lease is held")
}
func (panicCloudClient) CreateInstance(string, cloud.CreateOpts) (*cloud.Instance, error) {
	panic("cloud client should not be contacted while lease is held")
}
func (panicCloudClient) ShowInstance(string) (*cloud.Instance, error) {
	panic("cloud client should not be contacted while lease is held")
}
func (panicCloudClient) WaitReady(string, time.Duration) (*cloud.Instance, error) {
	panic("cloud client should not be contacted while lease is held")
}
func (panicCloudClient) ListAllInstances() ([]cloud.Instance, error) {
	panic("cloud client should not be contacted while lease is held")
}
func (panicCloudClient) DestroyInstance(string) error {
	panic("cloud client should not be contacted while lease is held")
}
func (panicCloudClient) CopyBetweenInstances(string, string, string, string) error {
	panic("cloud client should not be contacted while lease is held")
}
func (panicCloudClient) SelfDestructCmd(string) string { return "" }

func TestSyncCloudDefaultLeaseSkipsProviderWhenHeld(t *testing.T) {
	database := db.SetupTestDB(t)
	acquired, err := db.AcquireAutoLease(database, DefaultCloudSyncLeaseScope, "other-process", time.Minute)
	if err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	if !acquired {
		t.Fatal("seed lease was not acquired")
	}

	result := SyncCloud(nil, database, CloudSyncOptions{
		Clients:     []any{panicCloudClient{}},
		SyncResults: false,
	})
	if !result.Completed {
		t.Fatal("SyncCloud should complete DB-local fallback while lease is held")
	}
	if result.Updated != 0 {
		t.Fatalf("Updated = %d, want 0", result.Updated)
	}
}

func TestCompletedMarkerFallbackSafeRejectsCurrentlyRunningJob(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	const jobID = int64(1881)
	if _, err := db.UpsertLaunchLiveState(database, db.LaunchLiveState{
		LaunchID:      launchID,
		InstancePhase: "running:1881",
		UpdatedAt:     time.Now().Unix(),
	}); err != nil {
		t.Fatalf("UpsertLaunchLiveState: %v", err)
	}

	if completedMarkerFallbackSafe(database, db.StatusRunning, sqlNullInt64(launchID), jobID) {
		t.Fatal("fallback should be unsafe while live phase is running the same job")
	}
	if !completedMarkerFallbackSafe(database, db.StatusRunning, sqlNullInt64(launchID), 1884) {
		t.Fatal("fallback should be safe when live phase is running a different job")
	}
}

func sqlNullInt64(v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: true}
}
