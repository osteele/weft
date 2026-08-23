package ops

import (
	"context"
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
		wantStart int64
		wantEnd   int64
	}{
		{
			name:      "both times present",
			data:      `{"exit_code":0,"start_time":1700000001,"end_time":1700000011}`,
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
			start, end := parseCompletionRecordTimes([]byte(tt.data))
			if start != tt.wantStart || end != tt.wantEnd {
				t.Fatalf("parseCompletionRecordTimes = (%d, %d), want (%d, %d)", start, end, tt.wantStart, tt.wantEnd)
			}
		})
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
