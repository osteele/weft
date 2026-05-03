package cmd

import (
	"fmt"
	"testing"

	"github.com/osteele/weft/internal/db"
)

func TestRunJobPrioritySetsAndClearsPriority(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "python train.py", "priority")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}

	jobPriorityClear = false
	if err := runJobPriority(jobPriorityCmd, []string{fmt.Sprintf("wj%d", jobID)}); err != nil {
		t.Fatalf("runJobPriority set: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after set: %v", err)
	}
	if job.Priority != 1 {
		t.Fatalf("priority after set = %d, want 1", job.Priority)
	}

	jobPriorityClear = true
	t.Cleanup(func() { jobPriorityClear = false })
	if err := runJobPriority(jobPriorityCmd, []string{fmt.Sprintf("wj%d", jobID)}); err != nil {
		t.Fatalf("runJobPriority clear: %v", err)
	}
	job, err = db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID after clear: %v", err)
	}
	if job.Priority != 0 {
		t.Fatalf("priority after clear = %d, want 0", job.Priority)
	}
}
