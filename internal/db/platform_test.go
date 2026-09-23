package db

import "testing"

func TestPlatformSurvivesFreshAttempt(t *testing.T) {
	database := setupTestDB(t)
	id, err := RecordQueued(database, "host-alpha", "/tmp", "echo numerical-work", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetJobCLIResourceOverrides(database, id, &CLIResourceOverrides{Platform: "linux/amd64"}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"host-alpha", ""} {
		if _, err := CreateAttempt(database, id, target, nil, StatusQueued); err != nil {
			t.Fatal(err)
		}
		job, err := GetJobByID(database, id)
		if err != nil {
			t.Fatal(err)
		}
		if job.RequestedPlatform() != "linux/amd64" {
			t.Fatalf("platform dropped on retry to %q: %+v", target, job.CLIResourceOverrides)
		}
	}
}
