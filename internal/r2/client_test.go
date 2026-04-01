package r2

import (
	"fmt"
	"testing"
)

func jobStartedKey(jobID int64) string {
	return "jobs/" + itoa(jobID) + "/.started"
}

func jobCompleteKey(jobID int64) string {
	return "jobs/" + itoa(jobID) + "/.complete"
}

func jobAttemptStartedKey(jobID, runID int64) string {
	return "jobs/" + itoa(jobID) + "/runs/" + itoa(runID) + "/.started"
}

func jobAttemptCompleteKey(jobID, runID int64) string {
	return "jobs/" + itoa(jobID) + "/runs/" + itoa(runID) + "/.complete"
}

func itoa(n int64) string {
	return fmt.Sprintf("%d", n)
}

func TestJobMarkers_HasStartedMarkerMatchesExactCurrentRunKey(t *testing.T) {
	jobID := int64(222)
	currentRunID := int64(901)
	staleRunID := int64(700)

	markers := &JobMarkers{
		startedKeys: map[int64]map[string]struct{}{
			jobID: {
				jobAttemptStartedKey(jobID, staleRunID):   {},
				jobAttemptStartedKey(jobID, currentRunID): {},
			},
		},
	}

	if !markers.HasStartedMarker(jobID, jobAttemptStartedKey(jobID, currentRunID)) {
		t.Fatalf("expected current run marker to match")
	}
	if markers.HasStartedMarker(jobID, jobAttemptStartedKey(jobID, currentRunID+1)) {
		t.Fatalf("unexpected match for missing run marker")
	}
	if markers.HasStartedMarker(jobID, jobStartedKey(jobID)) {
		t.Fatalf("unexpected match for legacy top-level marker")
	}
}

func TestJobMarkers_HasCompletedMarkerMatchesExactCurrentRunKey(t *testing.T) {
	jobID := int64(223)
	currentRunID := int64(902)
	staleRunID := int64(701)

	markers := &JobMarkers{
		completedKeys: map[int64]map[string]struct{}{
			jobID: {
				jobAttemptCompleteKey(jobID, staleRunID):   {},
				jobAttemptCompleteKey(jobID, currentRunID): {},
			},
		},
	}

	if !markers.HasCompletedMarker(jobID, jobAttemptCompleteKey(jobID, currentRunID)) {
		t.Fatalf("expected current run completion marker to match")
	}
	if markers.HasCompletedMarker(jobID, jobAttemptCompleteKey(jobID, currentRunID+1)) {
		t.Fatalf("unexpected match for missing completion marker")
	}
	if markers.HasCompletedMarker(jobID, jobCompleteKey(jobID)) {
		t.Fatalf("unexpected match for legacy top-level completion marker")
	}
}

func TestJobMarkers_AnyCompletedKeyReturnsSomeKey(t *testing.T) {
	jobID := int64(441)
	runID := int64(555)

	markers := &JobMarkers{
		completedKeys: map[int64]map[string]struct{}{
			jobID: {
				jobAttemptCompleteKey(jobID, runID): {},
			},
		},
	}

	key, ok := markers.AnyCompletedKey(jobID)
	if !ok {
		t.Fatal("expected AnyCompletedKey to find a key")
	}
	if key != jobAttemptCompleteKey(jobID, runID) {
		t.Fatalf("AnyCompletedKey = %q, want %q", key, jobAttemptCompleteKey(jobID, runID))
	}

	_, ok = markers.AnyCompletedKey(999)
	if ok {
		t.Fatal("expected AnyCompletedKey to return false for unknown job")
	}
}
