package ops

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2keys"
)

type mapObjectGetter map[string][]byte

func (g mapObjectGetter) GetObject(_ context.Context, key string) ([]byte, error) {
	data, ok := g[key]
	if !ok {
		return nil, fmt.Errorf("missing %s", key)
	}
	return data, nil
}

func TestParseCompletionRecordTimes(t *testing.T) {
	tests := []struct {
		name      string
		data      string
		wantRun   int64
		wantStart int64
		wantEnd   int64
	}{
		{
			name:      "all fields present",
			data:      `{"run_id":42,"exit_code":0,"start_time":1700000001,"end_time":1700000011}`,
			wantRun:   42,
			wantStart: 1700000001,
			wantEnd:   1700000011,
		},
		{
			name:    "start_time absent (legacy record)",
			data:    `{"exit_code":0,"end_time":1700000011}`,
			wantEnd: 1700000011,
		},
		{
			name: "malformed json",
			data: `not json`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runID, start, end := parseCompletionRecordTimes([]byte(tt.data))
			if runID != tt.wantRun || start != tt.wantStart || end != tt.wantEnd {
				t.Fatalf("parseCompletionRecordTimes = (%d, %d, %d), want (%d, %d, %d)", runID, start, end, tt.wantRun, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestSyncJobStatusFromR2ScopesCompletionToCurrentAttempt(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "studio", "/tmp", "true", "inventory retry")
	if err != nil {
		t.Fatal(err)
	}
	firstJob, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if firstJob.LatestRunID == nil {
		t.Fatal("first attempt has no run ID")
	}
	firstRunID := *firstJob.LatestRunID
	if err := db.MarkQueuedJobRunning(database, jobID); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordCompletionByID(database, jobID, 1, 1700000011); err != nil {
		t.Fatal(err)
	}
	if err := db.RequeueByID(database, jobID); err != nil {
		t.Fatal(err)
	}
	currentJob, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if currentJob.LatestRunID == nil || *currentJob.LatestRunID == firstRunID {
		t.Fatalf("current run ID = %v, want a fresh attempt", currentJob.LatestRunID)
	}
	currentRunID := *currentJob.LatestRunID

	getter := mapObjectGetter{
		r2keys.JobAttemptComplete(jobID, firstRunID): []byte("1"),
		r2keys.JobAttemptResultsPrefix(jobID, firstRunID) + fmt.Sprintf("%d.completion.json", jobID): []byte(
			fmt.Sprintf(`{"run_id":%d,"start_time":1700000001,"end_time":1700000011}`, firstRunID),
		),
	}
	if _, err := syncJobStatusFromR2WithClient(context.Background(), getter, database, firstJob); !errors.Is(err, db.ErrCompletionAttemptNotCurrent) {
		t.Fatalf("stale attempt sync error = %v, want ErrCompletionAttemptNotCurrent", err)
	}
	unchanged, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != db.StatusQueued {
		t.Fatalf("current status = %q after stale completion, want queued", unchanged.Status)
	}
	if unchanged.StartTime != 0 {
		t.Fatalf("current start time = %d after stale completion, want unset", unchanged.StartTime)
	}

	getter[r2keys.JobAttemptComplete(jobID, currentRunID)] = []byte("0")
	currentResultKey := r2keys.JobAttemptResultsPrefix(jobID, currentRunID) + fmt.Sprintf("%d.completion.json", jobID)
	getter[currentResultKey] = []byte(
		fmt.Sprintf(`{"run_id":%d,"start_time":1700000020,"end_time":1700000030}`, firstRunID),
	)
	if _, err := syncJobStatusFromR2WithClient(context.Background(), getter, database, currentJob); err == nil {
		t.Fatal("mismatched completion record was accepted")
	}
	getter[currentResultKey] = []byte(
		fmt.Sprintf(`{"run_id":%d,"start_time":1700000020,"end_time":1700000030}`, currentRunID),
	)
	result, err := syncJobStatusFromR2WithClient(context.Background(), getter, database, currentJob)
	if err != nil {
		t.Fatalf("current attempt sync: %v", err)
	}
	if !result.Updated {
		t.Fatal("current attempt sync did not report an update")
	}
	completed, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != db.StatusCompleted || completed.ExitCode == nil || *completed.ExitCode != 0 {
		t.Fatalf("completed job = status %q exit %v", completed.Status, completed.ExitCode)
	}
	if completed.StartTime != 1700000020 {
		t.Fatalf("completed start time = %d, want 1700000020", completed.StartTime)
	}
}

func TestSyncInventoryPublicationReportsIngestsStandaloneReport(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "studio", "/tmp", "true", "inventory publication")
	if err != nil {
		t.Fatal(err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.LatestRunID == nil {
		t.Fatal("queued job has no attempt ID")
	}
	runID := *job.LatestRunID
	report := []byte(`{"sequence":2,"observed_at_unix":1700000000,"facets":{"execution_state":"complete","required_artifacts_state":"ready","drain_state":"pending"}}`)
	getter := mapObjectGetter{r2keys.JobAttemptPublicationReport(jobID, runID): report}

	if got := syncInventoryPublicationReportsWithClient(context.Background(), getter, database, []*db.Job{job}); got != 1 {
		t.Fatalf("updated reports = %d, want 1", got)
	}
	state, err := db.GetAttemptPublicationState(database, runID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RequiredArtifactsState != "ready" || state.DrainState != "pending" || state.Sequence != 2 {
		t.Fatalf("publication state = %+v", state)
	}
}
