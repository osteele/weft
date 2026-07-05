package cmd

import (
	"context"
	"database/sql"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestSetProcessedTagSingleWriterUsesMutationLayer(t *testing.T) {
	database := db.SetupTestDB(t)
	orig := setProcessedTagMutationFunc
	t.Cleanup(func() { setProcessedTagMutationFunc = orig })
	var gotJobID int64
	var gotProcessed bool
	setProcessedTagMutationFunc = func(_ context.Context, _ *sql.DB, jobID int64, processed bool) error {
		gotJobID = jobID
		gotProcessed = processed
		return nil
	}

	if err := setProcessedTagSingleWriter(database, 42, true); err != nil {
		t.Fatalf("setProcessedTagSingleWriter: %v", err)
	}
	if gotJobID != 42 || !gotProcessed {
		t.Fatalf("mutation call = job %d processed %v, want job 42 processed true", gotJobID, gotProcessed)
	}
}
