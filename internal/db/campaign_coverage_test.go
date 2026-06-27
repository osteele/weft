package db

import (
	"database/sql"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestCampaignFieldsRoundTrip(t *testing.T) {
	database := setupTestDB(t)

	id, err := CreateCampaign(database, &Campaign{
		Status:             CampaignStatusLaunching,
		EstimatedCostCents: 123,
		DistinctMachines:   true,
		AvoidMachines:      []string{"m-1", "m-2", "m-1", ""},
		AffinityMachines:   []string{"m-3", "m-4", "m-3", ""},
	})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	got, err := GetCampaign(database, id)
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	if got == nil {
		t.Fatal("GetCampaign returned nil")
	}
	if !got.DistinctMachines {
		t.Fatal("DistinctMachines = false, want true")
	}
	if !reflect.DeepEqual(got.AvoidMachines, []string{"m-1", "m-2"}) {
		t.Fatalf("AvoidMachines = %#v", got.AvoidMachines)
	}
	if !reflect.DeepEqual(got.AffinityMachines, []string{"m-3", "m-4"}) {
		t.Fatalf("AffinityMachines = %#v", got.AffinityMachines)
	}

	defaultID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusLaunching})
	if err != nil {
		t.Fatalf("CreateCampaign default: %v", err)
	}
	defaultCampaign, err := GetCampaign(database, defaultID)
	if err != nil {
		t.Fatalf("GetCampaign default: %v", err)
	}
	if defaultCampaign.DistinctMachines {
		t.Fatal("default DistinctMachines = true, want false")
	}
	if len(defaultCampaign.AvoidMachines) != 0 {
		t.Fatalf("default AvoidMachines = %#v, want empty", defaultCampaign.AvoidMachines)
	}
	if len(defaultCampaign.AffinityMachines) != 0 {
		t.Fatalf("default AffinityMachines = %#v, want empty", defaultCampaign.AffinityMachines)
	}
}

func TestCampaignCoverageMachineIDs(t *testing.T) {
	database := setupTestDB(t)
	campaignID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusRunning})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	otherCampaignID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusRunning})
	if err != nil {
		t.Fatalf("CreateCampaign other: %v", err)
	}

	completedLaunch := createCoverageLaunch(t, database, campaignID, LaunchStatusCompleted, "vastai", "covered")
	failedLaunch := createCoverageLaunch(t, database, campaignID, LaunchStatusFailed, "vastai", "failed")
	runningLaunch := createCoverageLaunch(t, database, campaignID, LaunchStatusRunning, "vastai", "inflight")
	emptyMachineLaunch := createCoverageLaunch(t, database, campaignID, LaunchStatusRunning, "vastai", "")
	otherLaunch := createCoverageLaunch(t, database, otherCampaignID, LaunchStatusCompleted, "vastai", "other")

	insertTestJob(t, database, 101, "echo covered", "/tmp", StatusCompleted, withLaunch(completedLaunch))
	setAttemptOutcome(t, database, 101, AttemptOutcomeCompleted)
	insertTestJob(t, database, 102, "echo failed", "/tmp", StatusFailed, withLaunch(failedLaunch))
	setAttemptOutcome(t, database, 102, AttemptOutcomeFailed)
	insertTestJob(t, database, 103, "echo running", "/tmp", StatusRunning, withLaunch(runningLaunch))
	insertTestJob(t, database, 104, "echo empty", "/tmp", StatusRunning, withLaunch(emptyMachineLaunch))
	insertTestJob(t, database, 105, "echo other", "/tmp", StatusCompleted, withLaunch(otherLaunch))
	setAttemptOutcome(t, database, 105, AttemptOutcomeCompleted)

	covered, err := CampaignCoveredMachineIDs(database, campaignID)
	if err != nil {
		t.Fatalf("CampaignCoveredMachineIDs: %v", err)
	}
	if !sameKeySet(covered, []string{"vastai/covered"}) {
		t.Fatalf("covered = %#v", covered)
	}

	inflight, err := CampaignInflightMachineIDs(database, campaignID)
	if err != nil {
		t.Fatalf("CampaignInflightMachineIDs: %v", err)
	}
	if !sameKeySet(inflight, []string{"vastai/inflight"}) {
		t.Fatalf("inflight = %#v", inflight)
	}
}

func TestResolveAvoidMachineIDs(t *testing.T) {
	database := setupTestDB(t)
	campaignID, err := CreateCampaign(database, &Campaign{Status: CampaignStatusRunning})
	if err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
	instanceID := createCoverageLaunch(t, database, campaignID, LaunchStatusCompleted, "vastai", "machine-from-instance")
	runpodInstanceID := createCoverageLaunch(t, database, campaignID, LaunchStatusCompleted, "runpod", "runpod-machine")
	firstJobLaunch := createCoverageLaunch(t, database, campaignID, LaunchStatusCompleted, "vastai", "old-job-machine")
	latestJobLaunch := createCoverageLaunch(t, database, campaignID, LaunchStatusCompleted, "vastai", "latest-job-machine")

	insertTestJob(t, database, 201, "echo first", "/tmp", StatusCompleted, withLaunch(firstJobLaunch))
	if _, err := CreateAttempt(database, 201, "", &latestJobLaunch, StatusCompleted); err != nil {
		t.Fatalf("CreateAttempt latest: %v", err)
	}
	insertTestJob(t, database, 202, "echo unresolved", "/tmp", StatusQueued)

	got, warnings, err := ResolveAvoidMachineIDs(database, []string{
		"raw-machine,wi" + itoa(instanceID),
		"wi" + itoa(runpodInstanceID),
		"wj201",
		"wj202",
		"wi999999",
	})
	if err != nil {
		t.Fatalf("ResolveAvoidMachineIDs: %v", err)
	}
	want := []string{"raw-machine", "vastai/machine-from-instance", "runpod/runpod-machine", "vastai/latest-job-machine"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolved = %#v, want %#v", got, want)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %#v, want 2 warnings", warnings)
	}
	if !strings.Contains(warnings[0], "wj202") || !strings.Contains(warnings[1], "wi999999") {
		t.Fatalf("warnings = %#v", warnings)
	}

	_, affinityWarnings, err := ResolveAffinityMachineIDs(database, []string{"wj202"})
	if err != nil {
		t.Fatalf("ResolveAffinityMachineIDs: %v", err)
	}
	if len(affinityWarnings) != 1 || !strings.Contains(affinityWarnings[0], "--affinity") {
		t.Fatalf("affinity warnings = %#v", affinityWarnings)
	}
}

func createCoverageLaunch(t *testing.T, database *sql.DB, campaignID int64, status, provider, machineID string) int64 {
	t.Helper()
	id, err := CreateLaunch(database, &Launch{
		CampaignID: &campaignID,
		Status:     status,
		Provider:   provider,
		MachineID:  machineID,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	return id
}

func setAttemptOutcome(t *testing.T, database *sql.DB, jobID int64, outcome string) {
	t.Helper()
	if _, err := database.Exec(
		`UPDATE job_attempts
		    SET cloud_outcome = ?
		  WHERE id = (SELECT id FROM job_attempts WHERE job_id = ? ORDER BY attempt_number DESC LIMIT 1)`,
		outcome,
		jobID,
	); err != nil {
		t.Fatalf("set attempt outcome: %v", err)
	}
}

func sameKeySet(got map[string]struct{}, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			return false
		}
	}
	return true
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}
