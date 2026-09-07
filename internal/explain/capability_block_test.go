package explain

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/opsqueue"
)

var missingRunnerCapabilityDetail = opsqueue.MissingRunnerCapabilityBlockDetail(
	opsqueue.CapabilityJobPayloadV1,
	"run `weft queue update host-alpha` to deploy a current agent",
)

func TestCapabilityBlockWithAlternativeTargetAllowsReplan(t *testing.T) {
	hosts := useCapabilityTestHosts(t)
	hosts[0].Capabilities = []string{"tool:agent-review", "agent:codex"}
	hosts[1].Capabilities = []string{" TOOL:AGENT-REVIEW ", " Agent:Codex "}
	hosts[2].Capabilities = []string{"tool:agent-review", "agent:codex"}
	hosts[2].AgentConcurrency = map[string]int{"*": 0, "codex": 1}
	setTestHosts(t, hosts)

	database := db.SetupTestDB(t)
	now := time.Unix(20_000, 0)
	job := recordCapabilityBlockedJob(t, database, now, []string{"tool:agent-review", "agent:codex"}, "nvidia")
	x := ForJob(database, job, now)

	if !x.AutoReplanAllowed {
		t.Fatal("AutoReplanAllowed = false, want true")
	}
	if !hasOption(x, "replan") {
		t.Fatalf("Options missing replan: %+v", x.Options)
	}
	if x.SuggestedAction != "replan: dispatch has been waiting for 20m" {
		t.Fatalf("SuggestedAction = %q", x.SuggestedAction)
	}
	if got, ok := evidenceValue(x, "capability alternatives"); !ok || got != "host-beta" {
		t.Fatalf("capability alternatives = %q, %v; want host-beta", got, ok)
	}
}

func TestCapabilityBlockRejectsCapabilityHostThatFailsDeviceConstraints(t *testing.T) {
	hosts := useCapabilityTestHosts(t)
	hosts[0].Capabilities = []string{"tool:agent-review"}
	hosts[1].Capabilities = []string{"tool:agent-review"}
	setTestHosts(t, hosts)

	database := db.SetupTestDB(t)
	now := time.Unix(20_000, 0)
	job := recordCapabilityBlockedJob(t, database, now, []string{"tool:agent-review"}, "a100")
	x := ForJob(database, job, now)

	if x.AutoReplanAllowed {
		t.Fatal("AutoReplanAllowed = true, want false")
	}
	if hasOption(x, "replan") {
		t.Fatalf("Options contain replan: %+v", x.Options)
	}
	want := "clear the dispatch block on host-alpha; this job's required capabilities are not satisfied elsewhere"
	if x.SuggestedAction != want {
		t.Fatalf("SuggestedAction = %q, want %q", x.SuggestedAction, want)
	}
	if got, ok := evidenceValue(x, "capability alternatives"); !ok || got != "none in inventory" {
		t.Fatalf("capability alternatives = %q, %v; want none in inventory", got, ok)
	}
}

// A job declaring nothing keeps the ordinary replan advice, so the guard does
// not suppress a suggestion that is still correct.
func TestJobWithoutCapabilitiesHasNoRequirement(t *testing.T) {
	for _, job := range []*db.Job{
		{},
		{Metadata: &db.JobMetadata{}},
		{Metadata: &db.JobMetadata{Agent: &db.JobAgentMetadata{}}},
	} {
		if got := requiredCapabilities(job); len(got) != 0 {
			t.Errorf("requiredCapabilities = %v, want empty", got)
		}
	}
}

func TestCapabilityBlockWithoutAlternativeTargetWaits(t *testing.T) {
	hosts := useCapabilityTestHosts(t)
	hosts[0].Capabilities = []string{"tool:agent-review"}
	setTestHosts(t, hosts)

	database := db.SetupTestDB(t)
	now := time.Unix(20_000, 0)
	job := recordCapabilityBlockedJob(t, database, now, []string{"tool:agent-review"}, "nvidia")
	x := ForJob(database, job, now)

	if x.AutoReplanAllowed {
		t.Fatal("AutoReplanAllowed = true, want false")
	}
	if hasOption(x, "replan") {
		t.Fatalf("Options contain replan: %+v", x.Options)
	}
	want := "clear the dispatch block on host-alpha; this job's required capabilities are not satisfied elsewhere"
	if x.SuggestedAction != want {
		t.Fatalf("SuggestedAction = %q, want %q", x.SuggestedAction, want)
	}
	if got, ok := evidenceValue(x, "capability alternatives"); !ok || got != "none in inventory" {
		t.Fatalf("capability alternatives = %q, %v; want none in inventory", got, ok)
	}
	if _, ok := evidenceValue(x, "inventory"); ok {
		t.Fatalf("unexpected inventory error evidence: %+v", x.Evidence)
	}
	if len(x.Options) != 1 || x.Options[0].Label != "wait" {
		t.Fatalf("Options = %+v, want wait only", x.Options)
	}
}

func TestCapabilityBlockWithUnavailableInventoryDoesNotClaimAbsence(t *testing.T) {
	inventory.UseTestHosts(t)
	restoreHosts := inventory.SetHosts(nil)
	t.Cleanup(restoreHosts)
	notDirectory := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(notDirectory, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	restoreHostsDir := inventory.SetHostsDir(notDirectory)
	t.Cleanup(restoreHostsDir)

	database := db.SetupTestDB(t)
	now := time.Unix(20_000, 0)
	job := recordCapabilityBlockedJob(t, database, now, []string{"tool:agent-review"}, "nvidia")
	x := ForJob(database, job, now)

	if x.AutoReplanAllowed {
		t.Fatal("AutoReplanAllowed = true, want false")
	}
	if hasOption(x, "replan") {
		t.Fatalf("Options contain replan: %+v", x.Options)
	}
	want := "clear the dispatch block on host-alpha; required capabilities follow the job to the unplaced pool, so replan can only reach hosts advertising them"
	if x.SuggestedAction != want {
		t.Fatalf("SuggestedAction = %q, want %q", x.SuggestedAction, want)
	}
	if strings.Contains(x.SuggestedAction, "not satisfied elsewhere") {
		t.Fatalf("SuggestedAction claims confirmed absence: %q", x.SuggestedAction)
	}
	if got, ok := evidenceValue(x, "inventory"); !ok || !strings.HasPrefix(got, "unavailable: load host inventory:") {
		t.Fatalf("inventory evidence = %q, %v; want unavailable lookup detail", got, ok)
	}
	if len(x.Options) != 1 || x.Options[0].Label != "wait" {
		t.Fatalf("Options = %+v, want wait only", x.Options)
	}
	if _, ok := evidenceValue(x, "capability alternatives"); ok {
		t.Fatalf("capability alternatives assert a result for an unavailable inventory: %+v", x.Evidence)
	}
	if _, ok := evidenceValue(x, "requires"); !ok {
		t.Fatalf("missing known requirement evidence: %+v", x.Evidence)
	}
	if x.Confidence != "high" {
		t.Fatalf("Confidence = %q, want high for the recorded dispatch block", x.Confidence)
	}
	if !strings.Contains(x.PrimaryReason, "queue runner lacks") {
		t.Fatalf("PrimaryReason = %q, want recorded capability block", x.PrimaryReason)
	}
}

func TestCapabilityBlockWithUnavailableActiveJobsDoesNotClaimAbsence(t *testing.T) {
	hosts := useCapabilityTestHosts(t)
	hosts[0].Capabilities = []string{"tool:agent-review"}
	hosts[1].Capabilities = []string{"tool:agent-review"}
	setTestHosts(t, hosts)

	database := db.SetupTestDB(t)
	now := time.Unix(20_000, 0)
	job := recordCapabilityBlockedJob(t, database, now, []string{"tool:agent-review"}, "nvidia")
	if _, err := database.Exec(`DROP VIEW job_status`); err != nil {
		t.Fatalf("drop job_status: %v", err)
	}
	x := ForJob(database, job, now)

	if x.AutoReplanAllowed {
		t.Fatal("AutoReplanAllowed = true, want false")
	}
	if hasOption(x, "replan") {
		t.Fatalf("Options contain replan: %+v", x.Options)
	}
	want := "clear the dispatch block on host-alpha; required capabilities follow the job to the unplaced pool, so replan can only reach hosts advertising them"
	if x.SuggestedAction != want {
		t.Fatalf("SuggestedAction = %q, want %q", x.SuggestedAction, want)
	}
	if got, ok := evidenceValue(x, "inventory"); !ok || !strings.Contains(got, "unavailable: check host host-beta constraints: list active jobs on host-beta:") {
		t.Fatalf("inventory evidence = %q, %v; want unavailable active-job lookup detail", got, ok)
	}
	if _, ok := evidenceValue(x, "capability alternatives"); ok {
		t.Fatalf("capability alternatives assert a result for an unavailable inventory: %+v", x.Evidence)
	}
}

func useCapabilityTestHosts(t *testing.T) []inventory.HostSpec {
	t.Helper()
	inventory.UseTestHosts(t)
	hosts, err := inventory.LoadHosts()
	if err != nil {
		t.Fatalf("LoadHosts: %v", err)
	}
	return hosts
}

func setTestHosts(t *testing.T, hosts []inventory.HostSpec) {
	t.Helper()
	restore := inventory.SetHosts(hosts)
	t.Cleanup(restore)
}

func recordCapabilityBlockedJob(t *testing.T, database *sql.DB, now time.Time, required []string, gpuClass string) *db.Job {
	t.Helper()
	jobID, err := db.RecordQueued(database, "host-alpha", "/tmp/project", "python train.py", "queued")
	if err != nil {
		t.Fatalf("RecordQueued: %v", err)
	}
	if err := db.SetJobGPUClass(database, jobID, gpuClass); err != nil {
		t.Fatalf("SetJobGPUClass: %v", err)
	}
	if err := db.SetJobMetadata(database, jobID, &db.JobMetadata{
		Agent: &db.JobAgentMetadata{RequiredCapabilities: required},
	}); err != nil {
		t.Fatalf("SetJobMetadata: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET queued_at = ? WHERE job_id = ?`, now.Add(-time.Hour).Unix(), jobID); err != nil {
		t.Fatalf("set queued_at: %v", err)
	}
	if err := db.InsertLifecycleEvent(database, &db.LifecycleEvent{
		OccurredAt: now.Add(-20 * time.Minute).Unix(),
		EventKind:  db.EventQueueDispatchDeferred,
		JobID:      jobID,
		Detail:     missingRunnerCapabilityDetail,
	}); err != nil {
		t.Fatalf("InsertLifecycleEvent: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("GetJobByID: %v", err)
	}
	return job
}

func hasOption(x Explanation, label string) bool {
	for _, option := range x.Options {
		if option.Label == label {
			return true
		}
	}
	return false
}
