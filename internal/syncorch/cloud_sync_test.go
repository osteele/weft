package syncorch

import (
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
