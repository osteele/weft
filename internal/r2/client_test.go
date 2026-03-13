package r2

import (
	"testing"

	"github.com/osteele/weft/internal/r2keys"
)

func TestJobMarkers_HasStartedMarkerMatchesExactCurrentRunKey(t *testing.T) {
	jobID := int64(222)
	currentRunID := int64(901)
	staleRunID := int64(700)

	markers := &JobMarkers{
		startedKeys: map[int64]map[string]struct{}{
			jobID: {
				r2keys.JobAttemptStarted(jobID, staleRunID):   {},
				r2keys.JobAttemptStarted(jobID, currentRunID): {},
			},
		},
	}

	if !markers.HasStartedMarker(jobID, r2keys.JobAttemptStarted(jobID, currentRunID)) {
		t.Fatalf("expected current run marker to match")
	}
	if markers.HasStartedMarker(jobID, r2keys.JobAttemptStarted(jobID, currentRunID+1)) {
		t.Fatalf("unexpected match for missing run marker")
	}
	if markers.HasStartedMarker(jobID, r2keys.JobStarted(jobID)) {
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
				r2keys.JobAttemptComplete(jobID, staleRunID):   {},
				r2keys.JobAttemptComplete(jobID, currentRunID): {},
			},
		},
	}

	if !markers.HasCompletedMarker(jobID, r2keys.JobAttemptComplete(jobID, currentRunID)) {
		t.Fatalf("expected current run completion marker to match")
	}
	if markers.HasCompletedMarker(jobID, r2keys.JobAttemptComplete(jobID, currentRunID+1)) {
		t.Fatalf("unexpected match for missing completion marker")
	}
	if markers.HasCompletedMarker(jobID, r2keys.JobComplete(jobID)) {
		t.Fatalf("unexpected match for legacy top-level completion marker")
	}
}
