package campaign

import (
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
)

func TestCloudSourceProvenanceIsAttemptScoped(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "true", "source provenance")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobMetadata(database, jobID, &db.JobMetadata{Source: &db.JobSourceMetadata{
		Hash: "manifest-a",
		Pin:  &db.JobSourcePinMetadata{Hash: "manifest-a", Roots: []db.JobSourcePinRootMetadata{{Hash: "root-a", R2Key: "root.tar.gz"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatal(err)
	}
	job, _ := db.GetJobByID(database, jobID)
	if err := persistCloudSourceDispatch(database, job, "agent-a"); err != nil {
		t.Fatal(err)
	}
	attempts, err := db.ListAttempts(database, jobID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, err = %v", attempts, err)
	}
	attemptID := attempts[0].ID
	persistCloudSourceVerification(database, jobID, attemptID, "", time.Unix(123, 0))
	attempts, _ = db.ListAttempts(database, jobID)
	execution := attempts[0].Metadata.Source.Execution
	if execution.Verification != db.SourceVerificationVerified || execution.VerifiedSHA256 != "manifest-a" || execution.VerifiedAt != 123 || execution.AgentVersion != "agent-a" {
		t.Fatalf("execution = %+v", execution)
	}
}

func TestCloudSourceRestoreFailureIsNotReportedVerified(t *testing.T) {
	database := db.SetupTestDB(t)
	jobID, err := db.RecordQueued(database, "", "/tmp/project", "true", "source provenance")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobMetadata(database, jobID, &db.JobMetadata{Source: &db.JobSourceMetadata{
		Pin: &db.JobSourcePinMetadata{Hash: "manifest-a", Roots: []db.JobSourcePinRootMetadata{{Hash: "root-a"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	launchID, err := db.CreateLaunch(database, &db.Launch{Status: db.LaunchStatusLaunching})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetJobLaunchID(database, jobID, launchID); err != nil {
		t.Fatal(err)
	}
	job, _ := db.GetJobByID(database, jobID)
	if err := persistCloudSourceDispatch(database, job, "agent-a"); err != nil {
		t.Fatal(err)
	}
	attempts, _ := db.ListAttempts(database, jobID)
	persistCloudSourceVerification(database, jobID, attempts[0].ID, "source_restore_failed: object missing", time.Unix(456, 0))
	attempts, _ = db.ListAttempts(database, jobID)
	if got := attempts[0].Metadata.Source.Execution.Verification; got != db.SourceVerificationObjectsUnavailable {
		t.Fatalf("verification = %q", got)
	}
}
