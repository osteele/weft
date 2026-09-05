package db

import (
	"database/sql"
	"testing"
)

func attemptMetadata(t *testing.T, database *sql.DB, attemptID int64) *JobMetadata {
	t.Helper()
	var raw sql.NullString
	if err := database.QueryRow(
		`SELECT job_metadata FROM job_attempts WHERE id = ?`, attemptID,
	).Scan(&raw); err != nil {
		t.Fatalf("read attempt metadata: %v", err)
	}
	return decodeJobMetadata(raw)
}

// A declared capability requirement must survive into a new attempt.
//
// When it does not, placement is not merely relaxed: the requirement is absent,
// so nothing evaluates it and the job is placed as though it had never
// constrained anything. That is how a job requiring host-installed tooling was
// replanned onto a rental and exited 127 without ever running.
func TestRequiredCapabilitiesSurviveANewAttempt(t *testing.T) {
	database := setupTestDB(t)
	const jobID = 4001
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusQueued)

	first, err := CreateAttempt(database, jobID, "host-alpha", nil, StatusQueued)
	if err != nil {
		t.Fatalf("create first attempt: %v", err)
	}
	want := []string{"tool:agent-review", "agent:codex"}
	if err := SetJobAttemptMetadata(database, jobID, first, &JobMetadata{
		Agent: &JobAgentMetadata{RequiredCapabilities: want},
	}); err != nil {
		t.Fatalf("set metadata: %v", err)
	}

	second, err := CreateAttempt(database, jobID, "", nil, StatusQueued)
	if err != nil {
		t.Fatalf("create second attempt: %v", err)
	}

	meta := attemptMetadata(t, database, second)
	if meta == nil || meta.Agent == nil {
		t.Fatal("the replanned attempt carries no agent metadata: the declared capability " +
			"requirement was dropped, so placement has nothing to evaluate and any host matches")
	}
	got := meta.Agent.RequiredCapabilities
	if len(got) != len(want) {
		t.Fatalf("required capabilities = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("capability %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// Carrying constraints forward must not invent one for a job that declared none.
func TestNoAgentMetadataIsNotFabricated(t *testing.T) {
	database := setupTestDB(t)
	const jobID = 4002
	insertTestJob(t, database, jobID, "echo test", "/tmp", StatusQueued)

	if _, err := CreateAttempt(database, jobID, "host-alpha", nil, StatusQueued); err != nil {
		t.Fatalf("create first attempt: %v", err)
	}
	second, err := CreateAttempt(database, jobID, "", nil, StatusQueued)
	if err != nil {
		t.Fatalf("create second attempt: %v", err)
	}
	if meta := attemptMetadata(t, database, second); meta != nil && meta.Agent != nil {
		t.Fatalf("agent metadata invented for a job that declared none: %v", meta.Agent)
	}
}
