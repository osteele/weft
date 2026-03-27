package db

import (
	"testing"
	"time"
)

func TestInsertAndListLifecycleEvents(t *testing.T) {
	database := SetupTestDB(t)

	// Insert a few events
	events := []LifecycleEvent{
		{EventKind: EventRelaunchEligible, JobCount: 4},
		{EventKind: EventRelaunchSkippedNoOffers, GPUSpec: "4090 ≥20GB", JobCount: 4},
		{EventKind: EventRetryAutoTriggered, LaunchID: 42, GPUSpec: "4090 ≥20GB", Detail: "bootstrap_timeout"},
		{EventKind: EventRetryNoOffers, AttemptNumber: 1, MaxAttempts: 5, Detail: "backoff 30s"},
		{EventKind: EventReconcileBootstrapTimeout, LaunchID: 42, GPUSpec: "4090 ≥20GB"},
	}
	for i := range events {
		events[i].OccurredAt = time.Now().Unix() - int64(len(events)-i)
		if err := InsertLifecycleEvent(database, &events[i]); err != nil {
			t.Fatalf("insert event %d: %v", i, err)
		}
	}

	// List all
	all, err := ListLifecycleEvents(database, LifecycleEventFilter{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 5 {
		t.Errorf("expected 5 events, got %d", len(all))
	}

	// Filter by kind prefix
	relaunchEvents, err := ListLifecycleEvents(database, LifecycleEventFilter{KindPrefix: "relaunch."})
	if err != nil {
		t.Fatalf("list relaunch: %v", err)
	}
	if len(relaunchEvents) != 2 {
		t.Errorf("expected 2 relaunch events, got %d", len(relaunchEvents))
	}

	// Filter by exact kind
	noOfferEvents, err := ListLifecycleEvents(database, LifecycleEventFilter{Kind: EventRelaunchSkippedNoOffers})
	if err != nil {
		t.Fatalf("list no_offers: %v", err)
	}
	if len(noOfferEvents) != 1 {
		t.Errorf("expected 1 no_offers event, got %d", len(noOfferEvents))
	}

	// Filter by launch ID
	launchEvents, err := ListLifecycleEvents(database, LifecycleEventFilter{LaunchID: 42})
	if err != nil {
		t.Fatalf("list launch 42: %v", err)
	}
	if len(launchEvents) != 2 {
		t.Errorf("expected 2 events for launch 42, got %d", len(launchEvents))
	}

	// Verify fields roundtrip
	if noOfferEvents[0].GPUSpec != "4090 ≥20GB" {
		t.Errorf("gpu_spec = %q, want %q", noOfferEvents[0].GPUSpec, "4090 ≥20GB")
	}
	if noOfferEvents[0].JobCount != 4 {
		t.Errorf("job_count = %d, want 4", noOfferEvents[0].JobCount)
	}
}

func TestInsertLifecycleEventNilDB(t *testing.T) {
	// Should not panic
	err := InsertLifecycleEvent(nil, &LifecycleEvent{EventKind: EventRelaunchEligible})
	if err != nil {
		t.Errorf("expected nil error for nil db, got %v", err)
	}
}
