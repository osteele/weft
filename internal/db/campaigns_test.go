package db

import "testing"

func TestGetMostRecentCampaign(t *testing.T) {
	database := SetupTestDB(t)

	firstID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusRunning})
	if err != nil {
		t.Fatalf("CreateCampaign(first): %v", err)
	}
	secondID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusFailed})
	if err != nil {
		t.Fatalf("CreateCampaign(second): %v", err)
	}

	got, err := GetMostRecentCampaign(database)
	if err != nil {
		t.Fatalf("GetMostRecentCampaign: %v", err)
	}
	if got == nil {
		t.Fatal("GetMostRecentCampaign returned nil")
	}
	if got.ID != secondID {
		t.Fatalf("most recent campaign = %d, want %d (first=%d)", got.ID, secondID, firstID)
	}
}

func TestGetMostRecentCampaignByStatus(t *testing.T) {
	database := SetupTestDB(t)

	if _, err := CreateCampaign(database, &Campaign{Status: CampaignStatusFailed}); err != nil {
		t.Fatalf("CreateCampaign(failed-1): %v", err)
	}
	if _, err := CreateCampaign(database, &Campaign{Status: CampaignStatusRunning}); err != nil {
		t.Fatalf("CreateCampaign(running): %v", err)
	}
	secondFailedID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusFailed})
	if err != nil {
		t.Fatalf("CreateCampaign(failed-2): %v", err)
	}

	got, err := GetMostRecentCampaignByStatus(database, CampaignStatusFailed)
	if err != nil {
		t.Fatalf("GetMostRecentCampaignByStatus: %v", err)
	}
	if got == nil {
		t.Fatal("GetMostRecentCampaignByStatus returned nil")
	}
	if got.ID != secondFailedID {
		t.Fatalf("most recent failed campaign = %d, want %d", got.ID, secondFailedID)
	}
}
