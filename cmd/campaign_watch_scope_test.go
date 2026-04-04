package cmd

import (
	"database/sql"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func createProjectQueuedJob(t *testing.T, database *sql.DB, project string) int64 {
	t.Helper()
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp/"+project, "python train.py", project+" job", "A100")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU(%s): %v", project, err)
	}
	if _, err := database.Exec(`UPDATE jobs SET project = ? WHERE id = ?`, project, jobID); err != nil {
		t.Fatalf("set project %s: %v", project, err)
	}
	return jobID
}

func TestResolveAllActiveWatchInstanceIDsReturnsRunningSet(t *testing.T) {
	database := db.SetupTestDB(t)

	runningID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "A100"})
	if err != nil {
		t.Fatalf("CreateLaunch(running): %v", err)
	}
	launchingID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching, Provider: "vastai", GPUSpec: "A100"})
	if err != nil {
		t.Fatalf("CreateLaunch(launching): %v", err)
	}
	if _, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusCompleted, Provider: "vastai", GPUSpec: "A100"}); err != nil {
		t.Fatalf("CreateLaunch(completed): %v", err)
	}

	got, err := resolveAllActiveWatchInstanceIDs(database, []int64{999})
	if err != nil {
		t.Fatalf("resolveAllActiveWatchInstanceIDs: %v", err)
	}
	if len(got) != 2 || got[0] != minInt64(runningID, launchingID) || got[1] != maxInt64(runningID, launchingID) {
		t.Fatalf("active IDs = %v, want [%d %d]", got, minInt64(runningID, launchingID), maxInt64(runningID, launchingID))
	}
}

func TestResolveCampaignWatchInstanceIDsIncludesCampaignAndReplacementChain(t *testing.T) {
	database := db.SetupTestDB(t)

	campaignID, err := db.CreateCampaign(database, &db.Campaign{Status: db.CampaignStatusLaunching})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	baseID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusRunning,
		Provider:   "vastai",
		GPUSpec:    "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(base): %v", err)
	}
	peerID, err := db.CreateLaunch(database, &db.Launch{
		CampaignID: &campaignID,
		Status:     db.LaunchStatusRunning,
		Provider:   "vastai",
		GPUSpec:    "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(peer): %v", err)
	}
	replacementID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "A100",
	})
	if err != nil {
		t.Fatalf("CreateLaunch(replacement): %v", err)
	}
	if err := db.SetLaunchReplacedID(database, replacementID, baseID); err != nil {
		t.Fatalf("SetLaunchReplacedID: %v", err)
	}

	got, err := resolveCampaignWatchInstanceIDs(database, []int64{baseID})
	if err != nil {
		t.Fatalf("resolveCampaignWatchInstanceIDs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("campaign watch IDs = %v, want 3 ids", got)
	}
	want := map[int64]bool{baseID: true, peerID: true, replacementID: true}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected campaign watch id %d in %v", id, got)
		}
		delete(want, id)
	}
	if len(want) > 0 {
		t.Fatalf("missing campaign watch IDs: %v (got %v)", want, got)
	}
}

func TestResolveProjectWatchInstanceIDsFiltersByProjectAndIncludesReplacement(t *testing.T) {
	database := db.SetupTestDB(t)

	alphaID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "A100"})
	if err != nil {
		t.Fatalf("CreateLaunch(alpha): %v", err)
	}
	betaID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "A100"})
	if err != nil {
		t.Fatalf("CreateLaunch(beta): %v", err)
	}
	alphaReplacementID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", GPUSpec: "A100"})
	if err != nil {
		t.Fatalf("CreateLaunch(alpha replacement): %v", err)
	}
	if err := db.SetLaunchReplacedID(database, alphaReplacementID, alphaID); err != nil {
		t.Fatalf("SetLaunchReplacedID(alpha): %v", err)
	}

	alphaJobID := createProjectQueuedJob(t, database, "ALPHA")
	betaJobID := createProjectQueuedJob(t, database, "BETA")
	if err := db.SetJobLaunchID(database, alphaJobID, alphaID); err != nil {
		t.Fatalf("SetJobLaunchID(alpha): %v", err)
	}
	if err := db.SetJobLaunchID(database, betaJobID, betaID); err != nil {
		t.Fatalf("SetJobLaunchID(beta): %v", err)
	}

	got, err := resolveProjectWatchInstanceIDs(database, "ALPHA", []int64{alphaID})
	if err != nil {
		t.Fatalf("resolveProjectWatchInstanceIDs: %v", err)
	}
	want := map[int64]bool{alphaID: true, alphaReplacementID: true}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("unexpected project watch id %d in %v", id, got)
		}
		delete(want, id)
	}
	if len(want) > 0 {
		t.Fatalf("missing project watch IDs: %v (got %v)", want, got)
	}
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
