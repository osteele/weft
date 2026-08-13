package db

import (
	"database/sql"
	"testing"
	"time"
)

func insertTestCampaign(t *testing.T, database *sql.DB, id int64, status string, createdAt int64, endedAt *int64) {
	t.Helper()
	if endedAt != nil {
		if _, err := database.Exec(
			`INSERT INTO campaigns (id, status, created_at, ended_at) VALUES (?, ?, ?, ?)`,
			id, status, createdAt, *endedAt,
		); err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := database.Exec(
		`INSERT INTO campaigns (id, status, created_at) VALUES (?, ?, ?)`,
		id, status, createdAt,
	); err != nil {
		t.Fatal(err)
	}
}

func insertWorkerLaunch(t *testing.T, database *sql.DB, id int64, campaignID int64, provider string, dataCenter string, createdAt int64) {
	t.Helper()
	if _, err := database.Exec(
		`INSERT INTO launches (id, campaign_id, status, provider, data_center, created_at, instance_role) VALUES (?, ?, ?, ?, ?, ?, 'worker')`,
		id, campaignID, LaunchStatusLaunching, provider, dataCenter, createdAt,
	); err != nil {
		t.Fatal(err)
	}
}

func TestComputeFirstRegistrationSurvival_EmptyUsesDefaults(t *testing.T) {
	database := SetupTestDB(t)
	defer database.Close()

	s, err := ComputeFirstRegistrationSurvival(database, FirstRegistrationScope{})
	if err != nil {
		t.Fatal(err)
	}
	if s.SampleSize != 0 {
		t.Fatalf("sample size = %d, want 0", s.SampleSize)
	}
	if s.ScopeLevel != FirstRegistrationScopeGlobal {
		t.Fatalf("scope = %s, want global", s.ScopeLevel)
	}
	if s.WarnAfter != 5*time.Minute {
		t.Fatalf("warn = %v, want 5m", s.WarnAfter)
	}
	if s.TerminateAfter != 12*time.Minute {
		t.Fatalf("terminate = %v, want 12m", s.TerminateAfter)
	}
	if s.WarnLearned || s.TerminateLearned {
		t.Fatalf("empty history marked thresholds learned: %+v", s)
	}
}

func TestComputeFirstRegistrationSurvival_ScopeFallback(t *testing.T) {
	database := SetupTestDB(t)
	defer database.Close()

	base := int64(1_000_000)
	var launchID int64 = 1
	for i := 0; i < 10; i++ {
		cID := int64(i + 1)
		insertTestCampaign(t, database, cID, CampaignStatusRunning, base, nil)
		insertWorkerLaunch(t, database, launchID, cID, "vastai", "us-east", base+60)
		launchID++
	}
	// provider-level enough samples (30) but DC-level not enough (10)
	for i := 0; i < 20; i++ {
		cID := int64(100 + i + 1)
		insertTestCampaign(t, database, cID, CampaignStatusRunning, base, nil)
		insertWorkerLaunch(t, database, launchID, cID, "vastai", "eu-west", base+120)
		launchID++
	}

	s, err := ComputeFirstRegistrationSurvival(database, FirstRegistrationScope{
		Provider:   "vastai",
		DataCenter: "us-east",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.ScopeLevel != FirstRegistrationScopeProvider {
		t.Fatalf("scope = %s, want provider fallback", s.ScopeLevel)
	}
	if s.SampleSize != 30 {
		t.Fatalf("sample size = %d, want 30", s.SampleSize)
	}
}

func TestComputeFirstRegistrationSurvival_IncludesTerminalNoLaunchFailures(t *testing.T) {
	database := SetupTestDB(t)
	defer database.Close()

	base := int64(2_000_000)

	// 20 successful campaigns with first registration at 90s
	for i := 0; i < 20; i++ {
		cID := int64(i + 1)
		insertTestCampaign(t, database, cID, CampaignStatusRunning, base, nil)
		insertWorkerLaunch(t, database, int64(i+1), cID, "runpod", "us", base+90)
	}
	// 5 terminal campaigns with no worker launches (failures)
	for i := 0; i < 5; i++ {
		cID := int64(100 + i + 1)
		ended := base + int64(300+i*10)
		insertTestCampaign(t, database, cID, CampaignStatusFailed, base, &ended)
	}

	s, err := ComputeFirstRegistrationSurvival(database, FirstRegistrationScope{})
	if err != nil {
		t.Fatal(err)
	}
	if s.SampleSize != 25 {
		t.Fatalf("sample size = %d, want 25", s.SampleSize)
	}

	p, ok := s.ConditionalSuccess(95 * time.Second)
	if !ok {
		t.Fatal("expected conditional success to be available")
	}
	if p >= 1.0 {
		t.Fatalf("conditional success = %.3f, want < 1 due to failure observations", p)
	}
}

func TestFirstRegistrationSurvival_ConditionalSuccessMonotonic(t *testing.T) {
	database := SetupTestDB(t)
	defer database.Close()

	base := int64(3_000_000)

	// 30 successes with increasing times
	for i := 0; i < 30; i++ {
		cID := int64(i + 1)
		insertTestCampaign(t, database, cID, CampaignStatusRunning, base, nil)
		insertWorkerLaunch(t, database, int64(i+1), cID, "vastai", "us-east", base+int64(60+i*5))
	}
	// 10 failures (terminal no-launch)
	for i := 0; i < 10; i++ {
		cID := int64(200 + i + 1)
		ended := base + int64(240+i*15)
		insertTestCampaign(t, database, cID, CampaignStatusCancelled, base, &ended)
	}

	s, err := ComputeFirstRegistrationSurvival(database, FirstRegistrationScope{
		Provider:   "vastai",
		DataCenter: "us-east",
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.ScopeLevel != FirstRegistrationScopeProviderDataCenter {
		t.Fatalf("scope = %s, want provider_data_center", s.ScopeLevel)
	}

	pEarly, ok := s.ConditionalSuccess(30 * time.Second)
	if !ok {
		t.Fatal("expected conditional success at early elapsed")
	}
	pLate, ok := s.ConditionalSuccess(180 * time.Second)
	if !ok {
		t.Fatal("expected conditional success at later elapsed")
	}
	if pLate > pEarly {
		t.Fatalf("conditional success increased: early=%.3f late=%.3f", pEarly, pLate)
	}
}
