package db

import "testing"

func TestListHostsForTUIFiltersRentalHostsToActiveLaunches(t *testing.T) {
	database := SetupTestDB(t)

	onPremJobID, err := RecordQueuedWithGPU(database, "cool30", "/tmp", "echo 1", "", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU on-prem: %v", err)
	}
	if err := MarkQueuedJobRunning(database, onPremJobID); err != nil {
		t.Fatalf("MarkQueuedJobRunning on-prem: %v", err)
	}

	runningID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusRunning,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch running: %v", err)
	}
	failedID, err := CreateLaunch(database, &Launch{
		Status:   LaunchStatusFailed,
		Provider: "vastai",
	})
	if err != nil {
		t.Fatalf("CreateLaunch failed: %v", err)
	}

	failedRentalJobID, err := RecordQueuedWithGPU(database, LaunchHost(failedID), "/tmp", "echo 2", "", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU failed-rental: %v", err)
	}
	if err := SetJobLaunchID(database, failedRentalJobID, failedID); err != nil {
		t.Fatalf("SetJobLaunchID failed-rental: %v", err)
	}

	if err := SaveCachedHostInfo(database, &CachedHostInfo{Name: "studio", LastUpdated: 1}); err != nil {
		t.Fatalf("SaveCachedHostInfo studio: %v", err)
	}
	if err := SaveCachedHostInfo(database, &CachedHostInfo{Name: LaunchHost(failedID), LastUpdated: 1}); err != nil {
		t.Fatalf("SaveCachedHostInfo failed rental: %v", err)
	}

	hosts, err := ListHostsForTUI(database)
	if err != nil {
		t.Fatalf("ListHostsForTUI: %v", err)
	}

	hostSet := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		hostSet[host] = struct{}{}
	}

	if _, ok := hostSet["cool30"]; !ok {
		t.Fatalf("expected cool30 in hosts, got %v", hosts)
	}
	if _, ok := hostSet["studio"]; !ok {
		t.Fatalf("expected studio in hosts, got %v", hosts)
	}
	if _, ok := hostSet[LaunchHost(runningID)]; !ok {
		t.Fatalf("expected active rental host %q in hosts, got %v", LaunchHost(runningID), hosts)
	}
	if _, ok := hostSet[LaunchHost(failedID)]; ok {
		t.Fatalf("did not expect terminal rental host %q in hosts, got %v", LaunchHost(failedID), hosts)
	}
}
