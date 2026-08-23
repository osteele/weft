package placement

import (
	"database/sql"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
)

func recordAgentCapabilityJob(t *testing.T, database *sql.DB, host string, capabilities ...string) int64 {
	t.Helper()
	jobID, err := db.RecordQueued(database, host, "/tmp/project", "true", "agent worker")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetJobMetadata(database, jobID, &db.JobMetadata{
		Agent: &db.JobAgentMetadata{RequiredCapabilities: capabilities},
	}); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}
	return jobID
}

func agentCapabilityHost(limits map[string]int) inventory.HostSpec {
	return inventory.HostSpec{
		Name:             "studio",
		Capabilities:     []string{"agent:codex", "agent:gemini"},
		AgentConcurrency: limits,
	}
}

func requireCapabilityEligibility(t *testing.T, database *sql.DB, host inventory.HostSpec, capability string, selfJobID int64, wantEligible bool) {
	t.Helper()
	got := CheckHostConstraintsWithActiveJobs(database, host, Constraints{
		RequiredCapabilities: []string{capability},
		SelfJobID:            selfJobID,
	})
	if got.Eligible != wantEligible {
		t.Fatalf("eligibility for %s = %v (%v), want %v", capability, got.Eligible, got.Messages(), wantEligible)
	}
}

func TestCapabilitySlotsCombineSharedAndPerAgentLimits(t *testing.T) {
	database := db.SetupTestDB(t)
	host := agentCapabilityHost(map[string]int{"*": 2, "codex": 1, "gemini": 2})

	codexID := recordAgentCapabilityJob(t, database, host.Name, "agent:codex")
	requireCapabilityEligibility(t, database, host, "agent:codex", 0, false)
	requireCapabilityEligibility(t, database, host, "agent:gemini", 0, true)

	recordAgentCapabilityJob(t, database, host.Name, "agent:gemini")
	requireCapabilityEligibility(t, database, host, "agent:gemini", 0, false)

	if err := db.MarkQueuedJobRunning(database, codexID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	if err := db.RecordCompletionByID(database, codexID, 0, 0); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}
	requireCapabilityEligibility(t, database, host, "agent:codex", 0, true)
	requireCapabilityEligibility(t, database, host, "agent:gemini", 0, true)
}

func TestCapabilitySlotsCountEveryActiveStatus(t *testing.T) {
	database := db.SetupTestDB(t)
	host := agentCapabilityHost(map[string]int{"*": 4, "codex": 4})

	recordAgentCapabilityJob(t, database, host.Name, "agent:codex")
	startingID := recordAgentCapabilityJob(t, database, host.Name, "agent:codex")
	if err := db.MarkQueuedJobStarting(database, startingID); err != nil {
		t.Fatalf("MarkQueuedJobStarting: %v", err)
	}
	runningID := recordAgentCapabilityJob(t, database, host.Name, "agent:codex")
	if err := db.MarkQueuedJobRunning(database, runningID); err != nil {
		t.Fatalf("MarkQueuedJobRunning: %v", err)
	}
	pausedID := recordAgentCapabilityJob(t, database, host.Name, "agent:codex")
	if err := db.MarkPausedByID(database, pausedID); err != nil {
		t.Fatalf("MarkPausedByID: %v", err)
	}

	requireCapabilityEligibility(t, database, host, "agent:codex", 0, false)
	if err := db.RecordCompletionByID(database, runningID, 0, 0); err != nil {
		t.Fatalf("RecordCompletionByID: %v", err)
	}
	requireCapabilityEligibility(t, database, host, "agent:codex", 0, true)
}

func TestCapabilitySlotRaceUsesLowestJobID(t *testing.T) {
	database := db.SetupTestDB(t)
	host := agentCapabilityHost(map[string]int{"*": 1, "codex": 1})

	firstID := recordAgentCapabilityJob(t, database, host.Name, "agent:codex")
	secondID := recordAgentCapabilityJob(t, database, host.Name, "agent:codex")

	requireCapabilityEligibility(t, database, host, "agent:codex", firstID, true)
	requireCapabilityEligibility(t, database, host, "agent:codex", secondID, false)
}

func TestCapabilityAdmissionIsUncappedWhenLimitsAreAbsent(t *testing.T) {
	database := db.SetupTestDB(t)
	host := agentCapabilityHost(nil)
	recordAgentCapabilityJob(t, database, host.Name, "agent:codex")
	recordAgentCapabilityJob(t, database, host.Name, "agent:codex")

	requireCapabilityEligibility(t, database, host, "agent:codex", 0, true)
}
