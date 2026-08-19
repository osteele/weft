package db

import "testing"

// The submitter session is stored on the jobs table and is deliberately not
// part of jobSelectColumns, so a job read through the ordinary path carries an
// empty value until it is filled in. These pin the fill-in step rather than
// the setter, which would pass even if nothing ever populated the field.
func TestPopulateSubmitterSessionsFillsJobs(t *testing.T) {
	database := setupTestDB(t)
	insertTestJob(t, database, 7001, "echo attributed", "/tmp", StatusQueued)
	insertTestJob(t, database, 7002, "echo anonymous", "/tmp", StatusQueued)

	if err := SetJobSubmitterSession(database, 7001, "sess-abc-123"); err != nil {
		t.Fatalf("SetJobSubmitterSession: %v", err)
	}

	attributed, err := GetJobByID(database, 7001)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	anonymous, err := GetJobByID(database, 7002)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	jobs := []*Job{attributed, anonymous}

	if err := PopulateSubmitterSessions(database, jobs); err != nil {
		t.Fatalf("PopulateSubmitterSessions: %v", err)
	}
	if attributed.SubmitterSession != "sess-abc-123" {
		t.Fatalf("SubmitterSession = %q, want %q", attributed.SubmitterSession, "sess-abc-123")
	}
	// Unattributed submissions are normal, not an error, and must not inherit
	// a neighbour's session.
	if anonymous.SubmitterSession != "" {
		t.Fatalf("unattributed SubmitterSession = %q, want empty", anonymous.SubmitterSession)
	}
}

// The id is opaque: whatever was stored comes back byte-for-byte, with no
// parsing, validation, or normalization.
func TestPopulateSubmitterSessionsRoundTripsOpaquely(t *testing.T) {
	database := setupTestDB(t)
	want := map[int64]string{}
	var jobs []*Job
	for i, id := range []string{"plain", "UPPER-and-lower_99", "has spaces inside", "weird:/chars?&=#"} {
		jobID := int64(7100 + i)
		insertTestJob(t, database, jobID, "echo opaque", "/tmp", StatusQueued)
		if err := SetJobSubmitterSession(database, jobID, id); err != nil {
			t.Fatalf("SetJobSubmitterSession(%q): %v", id, err)
		}
		job, err := GetJobByID(database, jobID)
		if err != nil {
			t.Fatalf("GetJobByID: %v", err)
		}
		want[jobID] = id
		jobs = append(jobs, job)
	}

	if err := PopulateSubmitterSessions(database, jobs); err != nil {
		t.Fatalf("PopulateSubmitterSessions: %v", err)
	}
	for _, job := range jobs {
		if job.SubmitterSession != want[job.ID] {
			t.Fatalf("job %d SubmitterSession = %q, want %q unchanged", job.ID, job.SubmitterSession, want[job.ID])
		}
	}
}

func TestPopulateSubmitterSessionsEmptyInputIsNoOp(t *testing.T) {
	database := setupTestDB(t)
	if err := PopulateSubmitterSessions(database, nil); err != nil {
		t.Fatalf("PopulateSubmitterSessions(nil): %v", err)
	}
	if err := PopulateSubmitterSessions(database, []*Job{nil}); err != nil {
		t.Fatalf("PopulateSubmitterSessions([nil]): %v", err)
	}
}
