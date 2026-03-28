package campaign

import (
	"strconv"
	"testing"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

func TestExtractCampaignID(t *testing.T) {
	tests := []struct {
		label string
		want  int64
		ok    bool
	}{
		{"weft/c42", 42, true},
		{"weft-c99", 99, true},
		{"weft/c0", 0, true},
		{"weft/c", 0, false},
		{"other-label", 0, false},
		{"", 0, false},
		{"weft/42", 0, false},
		{"weft-42", 0, false},
	}
	for _, tt := range tests {
		got, ok := extractCampaignID(tt.label)
		if ok != tt.ok || got != tt.want {
			t.Errorf("extractCampaignID(%q) = (%d, %v), want (%d, %v)", tt.label, got, ok, tt.want, tt.ok)
		}
	}
}

func TestSweepOrphanedInstances_DestroysOrphanFromTerminalCampaign(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create a completed campaign
	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusCompleted})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	var destroyedIDs []string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{
				{ProviderID: "orphan-1", Status: cloud.ProviderStatusRunning, Label: labelForCampaign(campaignID)},
				{ProviderID: "other-2", Status: cloud.ProviderStatusRunning, Label: "unrelated"},
			}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedIDs = append(destroyedIDs, id)
			return nil
		},
	}

	destroyed, err := SweepOrphanedInstances(database, []cloud.Client{mockClient})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if destroyed != 1 {
		t.Errorf("destroyed = %d, want 1", destroyed)
	}
	if len(destroyedIDs) != 1 || destroyedIDs[0] != "orphan-1" {
		t.Errorf("destroyed IDs = %v, want [orphan-1]", destroyedIDs)
	}
}

func TestSweepOrphanedInstances_SkipsActiveCampaignTrackedInstances(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create an active campaign with a tracked instance
	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusRunning,
		Provider:   "vastai",
		GPUSpec:    "RTX_4090",
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	if err := db.SetLaunchProviderID(database, instanceID, "tracked-123"); err != nil {
		t.Fatalf("set provider id: %v", err)
	}

	destroyCalled := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{
				{ProviderID: "tracked-123", Status: cloud.ProviderStatusRunning, Label: labelForCampaign(campaignID)},
			}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyCalled = true
			return nil
		},
	}

	destroyed, err := SweepOrphanedInstances(database, []cloud.Client{mockClient})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if destroyed != 0 {
		t.Errorf("destroyed = %d, want 0", destroyed)
	}
	if destroyCalled {
		t.Error("DestroyInstance should not be called for tracked instances")
	}
}

func TestSweepOrphanedInstances_DestroysUntrackedInActiveCampaign(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Create an active campaign but the provider instance is NOT in the DB
	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusRunning})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	var destroyedID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{
				{ProviderID: "ghost-999", Status: cloud.ProviderStatusRunning, Label: labelForCampaign(campaignID)},
			}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			return nil
		},
	}

	destroyed, err := SweepOrphanedInstances(database, []cloud.Client{mockClient})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if destroyed != 1 {
		t.Errorf("destroyed = %d, want 1", destroyed)
	}
	if destroyedID != "ghost-999" {
		t.Errorf("destroyed ID = %q, want ghost-999", destroyedID)
	}
}

func TestSweepOrphanedInstances_SkipsUnlabeledInstances(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	destroyCalled := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{
				{ProviderID: "unlabeled-1", Status: cloud.ProviderStatusRunning, Label: ""},
				{ProviderID: "other-2", Status: cloud.ProviderStatusRunning, Label: "my-personal-instance"},
			}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyCalled = true
			return nil
		},
	}

	destroyed, err := SweepOrphanedInstances(database, []cloud.Client{mockClient})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if destroyed != 0 {
		t.Errorf("destroyed = %d, want 0", destroyed)
	}
	if destroyCalled {
		t.Error("DestroyInstance should not be called for unlabeled instances")
	}
}

func TestSweepOrphanedInstances_SkipsDestroyedProviderInstances(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusCompleted})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	destroyCalled := false
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{
				{ProviderID: "dead-1", Status: cloud.ProviderStatusDestroyed, Label: labelForCampaign(campaignID)},
			}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyCalled = true
			return nil
		},
	}

	destroyed, err := SweepOrphanedInstances(database, []cloud.Client{mockClient})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if destroyed != 0 {
		t.Errorf("destroyed = %d, want 0", destroyed)
	}
	if destroyCalled {
		t.Error("DestroyInstance should not be called for already-destroyed instances")
	}
}

func TestSweepOrphanedInstances_DestroysStoppedInstances(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Stopped instances still incur disk charges and must be destroyed
	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusCompleted})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}

	var destroyedID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{
				{ProviderID: "stopped-1", Status: cloud.ProviderStatusStopped, Label: labelForCampaign(campaignID)},
			}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			return nil
		},
	}

	destroyed, err := SweepOrphanedInstances(database, []cloud.Client{mockClient})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if destroyed != 1 {
		t.Errorf("destroyed = %d, want 1", destroyed)
	}
	if destroyedID != "stopped-1" {
		t.Errorf("destroyed ID = %q, want stopped-1", destroyedID)
	}
}

func TestSweepOrphanedInstances_NoCampaignInDB(t *testing.T) {
	database := setupTestDB(t)
	defer database.Close()

	// Campaign ID 999 does not exist in DB
	var destroyedID string
	mockClient := &cloud.MockClient{
		ProviderVal: cloud.ProviderVastai,
		ListAllInstancesFunc: func() ([]cloud.Instance, error) {
			return []cloud.Instance{
				{ProviderID: "stale-1", Status: cloud.ProviderStatusRunning, Label: "weft/c999"},
			}, nil
		},
		DestroyInstanceFunc: func(id string) error {
			destroyedID = id
			return nil
		},
	}

	destroyed, err := SweepOrphanedInstances(database, []cloud.Client{mockClient})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if destroyed != 1 {
		t.Errorf("destroyed = %d, want 1", destroyed)
	}
	if destroyedID != "stale-1" {
		t.Errorf("destroyed ID = %q, want stale-1", destroyedID)
	}
}

func labelForCampaign(id int64) string {
	return "weft/c" + strconv.FormatInt(id, 10)
}
