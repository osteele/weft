package db

import (
	"encoding/json"
	"errors"
	"testing"
)

func publicationInt64(v int64) *int64 { return &v }
func publicationInt(v int) *int       { return &v }

func TestAttemptPublicationStateMonotonicAndAttemptScoped(t *testing.T) {
	database := setupTestDB(t)
	const jobID = int64(4201)
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusQueued)
	attemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}

	initial := &AttemptPublicationState{
		AttemptID: attemptID, JobID: jobID, Sequence: 2, ObservedAt: 100,
		ExecutionState: PublicationExecutionComplete, ExecutionCompletedAt: publicationInt64(90),
		RequiredArtifactsState: PublicationStateReady, RequiredArtifactsReadyAt: publicationInt64(95),
		DrainState:  PublicationStatePending,
		QueuedItems: publicationInt(1), QueuedBytes: publicationInt64(128),
		Artifacts: []AttemptPublicationArtifact{{Name: "model", State: PublicationStateReady, ReadyAt: publicationInt64(95)}},
	}
	updated, err := UpsertAttemptPublicationState(database, initial)
	if err != nil || !updated {
		t.Fatalf("initial upsert = %v, %v", updated, err)
	}

	stale := *initial
	stale.Sequence = 1
	stale.QueuedBytes = publicationInt64(999)
	if updated, err := UpsertAttemptPublicationState(database, &stale); err != nil || updated {
		t.Fatalf("stale upsert = %v, %v; want ignored", updated, err)
	}

	newer := *initial
	newer.Sequence = 3
	newer.ObservedAt = 110
	newer.ExecutionState = PublicationExecutionUnknown
	newer.RequiredArtifactsState = PublicationStatePending
	newer.RequiredArtifactsReadyAt = nil
	newer.DrainState = PublicationStateReady
	newer.DrainCompletedAt = publicationInt64(109)
	newer.QueuedItems = publicationInt(0)
	newer.QueuedBytes = nil
	newer.Artifacts = []AttemptPublicationArtifact{{Name: "model", State: PublicationStateUnknown}}
	if updated, err := UpsertAttemptPublicationState(database, &newer); err != nil || !updated {
		t.Fatalf("newer upsert = %v, %v", updated, err)
	}

	got, err := GetLatestAttemptPublicationState(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sequence != 3 || got.ExecutionState != PublicationExecutionComplete {
		t.Fatalf("state = %+v; execution terminal state should be preserved", got)
	}
	if got.RequiredArtifactsState != PublicationStateReady || got.RequiredArtifactsReadyAt == nil || *got.RequiredArtifactsReadyAt != 95 {
		t.Fatalf("required artifacts = %q at %v; terminal state should be preserved", got.RequiredArtifactsState, got.RequiredArtifactsReadyAt)
	}
	if got.DrainState != PublicationStateReady || got.QueuedItems == nil || *got.QueuedItems != 0 {
		t.Fatalf("drain/snapshot = %+v", got)
	}
	if got.QueuedBytes != nil {
		t.Fatalf("queued bytes = %v, want unknown", got.QueuedBytes)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].State != PublicationStateReady {
		t.Fatalf("artifacts = %+v; terminal state should be preserved", got.Artifacts)
	}
}

func TestAttemptPublicationStateRejectsMismatchedJob(t *testing.T) {
	database := setupTestDB(t)
	insertTestJob(t, database, 4202, "true", "/tmp", StatusQueued)
	attemptID, err := GetLatestAttemptID(database, 4202)
	if err != nil {
		t.Fatal(err)
	}
	_, err = UpsertAttemptPublicationState(database, &AttemptPublicationState{
		AttemptID: attemptID, JobID: 9999, Sequence: 1, ObservedAt: 1,
		ExecutionState:         PublicationExecutionComplete,
		RequiredArtifactsState: PublicationStateUnknown,
		DrainState:             PublicationStateUnknown,
	})
	if !errors.Is(err, ErrPublicationAttemptMismatch) {
		t.Fatalf("error = %v, want ErrPublicationAttemptMismatch", err)
	}
}

func TestDecodeAttemptPublicationReportStandaloneAndCompletionEnvelope(t *testing.T) {
	report := json.RawMessage(`{
		"sequence": 4,
		"observed_at_unix": 200,
		"facets": {
			"execution_state": "complete",
			"execution_completed_at_unix": 180,
			"required_artifacts_state": "unknown",
			"drain_state": "pending",
			"unknown_reason": "R2 lookup timed out"
		},
		"snapshot": {"queued_items": 0, "queued_bytes": null},
		"artifacts": [{"name": "weights", "state": "unknown"}]
	}`)
	for _, data := range [][]byte{
		report,
		[]byte(`{"exit_code":0,"publication":` + string(report) + `}`),
	} {
		got, err := DecodeAttemptPublicationReport(data, 9, 11)
		if err != nil {
			t.Fatal(err)
		}
		if got.JobID != 9 || got.AttemptID != 11 || got.Sequence != 4 {
			t.Fatalf("identity = %+v", got)
		}
		if got.QueuedItems == nil || *got.QueuedItems != 0 || got.QueuedBytes != nil {
			t.Fatalf("nullable quantities = %+v", got)
		}
		if got.UnknownReason != "R2 lookup timed out" || len(got.Artifacts) != 1 {
			t.Fatalf("decoded report = %+v", got)
		}
	}
}

func TestMissingLegacyPublicationIsUnknownNotAbsent(t *testing.T) {
	database := setupTestDB(t)
	const jobID = int64(4203)
	insertTestJob(t, database, jobID, "true", "/tmp", StatusQueued)
	attemptID, err := GetLatestAttemptID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		UPDATE job_attempts SET status = ?, end_time = ? WHERE id = ?`,
		StatusCompleted, int64(300), attemptID); err != nil {
		t.Fatal(err)
	}
	got, err := GetLatestAttemptPublicationState(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("missing legacy report returned absent state")
	}
	if got.ExecutionState != PublicationExecutionComplete || got.RequiredArtifactsState != PublicationStateUnknown || got.DrainState != PublicationStateUnknown {
		t.Fatalf("legacy facets = %+v", got)
	}
	if got.UnknownReason == "" || got.Sequence != 0 {
		t.Fatalf("legacy provenance = %+v", got)
	}
}
