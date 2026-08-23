package remediation

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ssh"
)

func TestRemediatorCheckFailedJobsDrivesCandidateLoop(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "hostA", "/tmp", "python train.py", "failed")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.RecordCompletionByID(database, jobID, 1, time.Now().Unix()); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}
	candidates, err := db.ListRecentFailedUndiagnosed(database, 10)
	if err != nil {
		t.Fatalf("ListRecentFailedUndiagnosed: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != jobID {
		job, getErr := db.GetJobByID(database, jobID)
		if getErr != nil {
			t.Fatalf("GetJobByID: %v", getErr)
		}
		t.Fatalf("candidates = %+v, want job %d; derived status=%s exit=%v end=%d retry=%d diagnosis=%q",
			candidates, jobID, job.Status, job.ExitCode, job.EndTime, job.RetryCount, job.ErrorDiagnosis)
	}

	fetches := 0
	cleanupSSH := ssh.SetRunner(func(host, command string) (string, string, error) {
		fetches++
		if host != "hostA" {
			return "", "", fmt.Errorf("unexpected host %q", host)
		}
		return "", "", nil
	})
	t.Cleanup(cleanupSSH)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	remediator := NewRemediator(database, logger, config.DefaultConfig(), time.Second)

	if got := remediator.CheckFailedJobs(); got != 0 {
		t.Fatalf("CheckFailedJobs processed %d jobs, want 0", got)
	}
	if fetches != 1 {
		t.Fatalf("fetches = %d, want 1", fetches)
	}
}
