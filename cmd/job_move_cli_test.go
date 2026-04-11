package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/spf13/cobra"
)

func TestResolveMoveDestination_WithToFlag(t *testing.T) {
	dest, jobArgs, err := resolveMoveDestination([]string{"wj946", "wj943"}, "new")
	if err != nil {
		t.Fatalf("resolveMoveDestination returned error: %v", err)
	}
	if dest != "new" {
		t.Fatalf("destination = %q, want %q", dest, "new")
	}
	if len(jobArgs) != 2 || jobArgs[0] != "wj946" || jobArgs[1] != "wj943" {
		t.Fatalf("job args = %v, want [wj946 wj943]", jobArgs)
	}
}

func TestResolveMoveDestination_UsesFinalPositionalDestination(t *testing.T) {
	dest, jobArgs, err := resolveMoveDestination([]string{"wj946", "wj943", "new"}, "")
	if err != nil {
		t.Fatalf("resolveMoveDestination returned error: %v", err)
	}
	if dest != "new" {
		t.Fatalf("destination = %q, want %q", dest, "new")
	}
	if len(jobArgs) != 2 || jobArgs[0] != "wj946" || jobArgs[1] != "wj943" {
		t.Fatalf("job args = %v, want [wj946 wj943]", jobArgs)
	}
}

func TestNormalizeMoveDestination_DistinctEnablesEach(t *testing.T) {
	dest, each := normalizeMoveDestination("distinct", false)
	if dest != "new" {
		t.Fatalf("destination = %q, want %q", dest, "new")
	}
	if !each {
		t.Fatal("expected distinct destination to enable --each semantics")
	}
}

func TestNormalizeMoveDestination_OtherDestinationsUnchanged(t *testing.T) {
	dest, each := normalizeMoveDestination("wi872", false)
	if dest != "wi872" {
		t.Fatalf("destination = %q, want %q", dest, "wi872")
	}
	if each {
		t.Fatal("unexpected each=true for non-distinct destination")
	}
}

func TestValidateMoveSelectors_RejectsMixedSelectors(t *testing.T) {
	err := validateMoveSelectors([]string{"wj946"}, "myproj", "")
	if err == nil {
		t.Fatal("expected selector validation error")
	}
}

func TestValidateMoveSelectors_RequiresSelector(t *testing.T) {
	err := validateMoveSelectors(nil, "", "")
	if err == nil {
		t.Fatal("expected selector validation error")
	}
}

func TestResolveEligibleJobs_FromSelectsOnlyQueuedJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}

	queuedJobID, err := db.RecordQueued(database, "", t.TempDir(), "echo queued", "queued")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobLaunchID(database, queuedJobID, instanceID); err != nil {
		t.Fatalf("set launch id (queued): %v", err)
	}

	runningJobID, err := db.RecordQueued(database, "", t.TempDir(), "echo running", "running")
	if err != nil {
		t.Fatalf("record running job: %v", err)
	}
	if err := db.SetJobLaunchID(database, runningJobID, instanceID); err != nil {
		t.Fatalf("set launch id (running): %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, start_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusRunning, time.Now().Unix(), runningJobID,
	); err != nil {
		t.Fatalf("mark running attempt: %v", err)
	}

	jobs, err := resolveEligibleJobs(database, nil, "", ids.FormatInstanceID(instanceID), false)
	if err != nil {
		t.Fatalf("resolveEligibleJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("eligible jobs = %d, want 1", len(jobs))
	}
	if jobs[0].ID != queuedJobID {
		t.Fatalf("eligible job ID = %d, want %d", jobs[0].ID, queuedJobID)
	}
}

func TestMoveJobsVerbAlias_ExposesMoveFlags(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"move", "jobs"})
	if err != nil {
		t.Fatalf("find move jobs command: %v", err)
	}
	if cmd == nil {
		t.Fatal("move jobs command not found")
	}
	if cmd.Flags().Lookup("to") == nil {
		t.Fatal("--to flag not found on move jobs alias")
	}
	if cmd.Flags().Lookup("from") == nil {
		t.Fatal("--from flag not found on move jobs alias")
	}
	if cmd.Flags().Lookup("project") == nil {
		t.Fatal("--project flag not found on move jobs alias")
	}
	if cmd.Flags().Lookup("each") == nil {
		t.Fatal("--each flag not found on move jobs alias")
	}
}

func TestMoveRootAlias_ExposesMoveFlags(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"move"})
	if err != nil {
		t.Fatalf("find move command: %v", err)
	}
	if cmd == nil {
		t.Fatal("move command not found")
	}
	if cmd.Flags().Lookup("to") == nil {
		t.Fatal("--to flag not found on move command")
	}
	if cmd.Flags().Lookup("from") == nil {
		t.Fatal("--from flag not found on move command")
	}
	if cmd.Flags().Lookup("project") == nil {
		t.Fatal("--project flag not found on move command")
	}
	if cmd.Flags().Lookup("each") == nil {
		t.Fatal("--each flag not found on move command")
	}
}

func TestMoveRootAlias_RequiresSelectorLikeJobMove(t *testing.T) {
	err := runMove(moveCmd, []string{"new"})
	if err == nil {
		t.Fatal("expected selector validation error")
	}
	if !strings.Contains(err.Error(), "provide job IDs, --project, or --from") {
		t.Fatalf("error = %q, want selector guidance", err)
	}
}

func TestRunMove_AllowsFlagOnlySelectorForms(t *testing.T) {
	origFrom, origProject, origTo := jobMoveFrom, jobMoveProject, jobMoveTo
	origDelegate := runMoveDelegate
	t.Cleanup(func() {
		jobMoveFrom, jobMoveProject, jobMoveTo = origFrom, origProject, origTo
		runMoveDelegate = origDelegate
	})

	jobMoveFrom = "wi872"
	jobMoveTo = "new"

	sentinel := errors.New("delegate called")
	runMoveDelegate = func(cmd *cobra.Command, args []string) error {
		return sentinel
	}

	err := runMove(moveCmd, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected delegated execution, got %v", err)
	}
}
