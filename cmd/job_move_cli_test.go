package cmd

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/orchestration"
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

func TestMoveJobsToAuto_ReturnsCloudJobToUnplaced(t *testing.T) {
	database := db.SetupTestDB(t)

	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:   db.LaunchStatusRunning,
		Provider: "vastai",
		GPUSpec:  "RTX 4090",
	})
	if err != nil {
		t.Fatalf("create launch: %v", err)
	}
	jobID, err := db.RecordQueued(database, "", t.TempDir(), "echo queued", "queued")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set launch id: %v", err)
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}

	if err := moveJobsToAuto(database, []*db.Job{job}); err != nil {
		t.Fatalf("moveJobsToAuto returned error: %v", err)
	}

	moved, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get moved job: %v", err)
	}
	if moved.TargetKind() != db.JobTargetUnplaced {
		t.Fatalf("target kind = %s, want %s", moved.TargetKind(), db.JobTargetUnplaced)
	}
	if moved.LaunchID != nil {
		t.Fatalf("launch ID = %v, want nil", *moved.LaunchID)
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

func TestResolveEligibleJobs_FromHostSelectsOnlyEligibleQueuedJobs(t *testing.T) {
	database := db.SetupTestDB(t)

	queuedJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo queued", "queued")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}

	runningJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo running", "running")
	if err != nil {
		t.Fatalf("record running job: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, start_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusRunning, time.Now().Unix(), runningJobID,
	); err != nil {
		t.Fatalf("mark running attempt: %v", err)
	}

	otherHostJobID, err := db.RecordQueued(database, "cool100", t.TempDir(), "echo other host", "other host")
	if err != nil {
		t.Fatalf("record other-host job: %v", err)
	}

	jobs, err := resolveEligibleJobs(database, nil, "", "cool30", false)
	if err != nil {
		t.Fatalf("resolveEligibleJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("eligible jobs = %d, want 1", len(jobs))
	}
	if jobs[0].ID != queuedJobID {
		t.Fatalf("eligible job ID = %d, want %d", jobs[0].ID, queuedJobID)
	}

	if jobs[0].ID == runningJobID || jobs[0].ID == otherHostJobID {
		t.Fatalf("unexpected job in eligible set: got %d", jobs[0].ID)
	}
}

func TestResolveEligibleJobs_FromHostNoEligibleJobsReportsHost(t *testing.T) {
	database := db.SetupTestDB(t)

	runningJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo running", "running")
	if err != nil {
		t.Fatalf("record running job: %v", err)
	}
	if _, err := database.Exec(
		`UPDATE job_attempts SET status = ?, start_time = ? WHERE job_id = ? AND end_time IS NULL`,
		db.StatusRunning, time.Now().Unix(), runningJobID,
	); err != nil {
		t.Fatalf("mark running attempt: %v", err)
	}

	_, err = resolveEligibleJobs(database, nil, "", "cool30", false)
	if err == nil {
		t.Fatal("expected no-eligible-jobs error")
	}
	if !strings.Contains(err.Error(), "no eligible queued jobs found on host cool30") {
		t.Fatalf("error = %q, want host-specific empty selection message", err)
	}
}

func TestResolveEligibleJobs_FromFallsBackToProject(t *testing.T) {
	database := db.SetupTestDB(t)

	projectJobID, err := db.RecordQueued(database, "", t.TempDir(), "echo queued", "queued")
	if err != nil {
		t.Fatalf("record queued job: %v", err)
	}
	if err := db.SetJobProject(database, projectJobID, "markov-attention"); err != nil {
		t.Fatalf("set project: %v", err)
	}

	jobs, err := resolveEligibleJobs(database, nil, "", "markov-attention", false)
	if err != nil {
		t.Fatalf("resolveEligibleJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("eligible jobs = %d, want 1", len(jobs))
	}
	if jobs[0].ID != projectJobID {
		t.Fatalf("eligible job ID = %d, want %d", jobs[0].ID, projectJobID)
	}
}

func TestResolveEligibleJobs_FromHostTakesPrecedenceOverSameNamedProject(t *testing.T) {
	database := db.SetupTestDB(t)

	hostJobID, err := db.RecordQueued(database, "cool30", t.TempDir(), "echo host", "host")
	if err != nil {
		t.Fatalf("record host job: %v", err)
	}
	projectJobID, err := db.RecordQueued(database, "", t.TempDir(), "echo project", "project")
	if err != nil {
		t.Fatalf("record project job: %v", err)
	}
	if err := db.SetJobProject(database, projectJobID, "cool30"); err != nil {
		t.Fatalf("set project: %v", err)
	}

	jobs, err := resolveEligibleJobs(database, nil, "", "cool30", false)
	if err != nil {
		t.Fatalf("resolveEligibleJobs returned error: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("eligible jobs = %d, want 1", len(jobs))
	}
	if jobs[0].ID != hostJobID {
		t.Fatalf("eligible job ID = %d, want host job %d", jobs[0].ID, hostJobID)
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
	if cmd.Flags().Lookup("tui") == nil {
		t.Fatal("--tui flag not found on move jobs alias")
	}
	if cmd.Flags().Lookup("plain") == nil {
		t.Fatal("--plain flag not found on move jobs alias")
	}
	if cmd.Flags().Lookup("no-tui") == nil {
		t.Fatal("--no-tui compatibility flag not found on move jobs alias")
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
	if cmd.Flags().Lookup("tui") == nil {
		t.Fatal("--tui flag not found on move command")
	}
	if cmd.Flags().Lookup("plain") == nil {
		t.Fatal("--plain flag not found on move command")
	}
	if cmd.Flags().Lookup("no-tui") == nil {
		t.Fatal("--no-tui compatibility flag not found on move command")
	}
}

func TestJobMove_ExposesTUIModeFlags(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"job", "move"})
	if err != nil {
		t.Fatalf("find job move command: %v", err)
	}
	if cmd == nil {
		t.Fatal("job move command not found")
	}
	if cmd.Flags().Lookup("tui") == nil {
		t.Fatal("--tui flag not found on job move")
	}
	if cmd.Flags().Lookup("plain") == nil {
		t.Fatal("--plain flag not found on job move")
	}
	if flag := cmd.Flags().Lookup("no-tui"); flag == nil {
		t.Fatal("--no-tui compatibility flag not found on job move")
	} else if !flag.Hidden {
		t.Fatal("--no-tui should be hidden")
	}
}

func TestJobMove_TUIModeFlagsConflict(t *testing.T) {
	origTUI := jobMoveTUI
	origPlain := jobMovePlain
	origNoTUI := jobMoveNoTUI
	t.Cleanup(func() {
		jobMoveTUI = origTUI
		jobMovePlain = origPlain
		jobMoveNoTUI = origNoTUI
	})

	tests := []struct {
		name string
		args []string
	}{
		{name: "tui and plain", args: []string{"--tui", "--plain"}},
		{name: "tui and no-tui", args: []string{"--tui", "--no-tui"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var each bool
			var project string
			var dest string
			var from string
			cmd := &cobra.Command{
				Use: "move-test",
				RunE: func(_ *cobra.Command, _ []string) error {
					return nil
				},
			}
			addJobMoveFlags(cmd, &each, &project, &dest, &from)
			cmd.SetArgs(tt.args)
			if err := cmd.Execute(); err == nil {
				t.Fatal("expected mutually exclusive flag error")
			}
		})
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

func TestMoveJobsToNewInstancesRetriesSingleJobRetryableLaunch(t *testing.T) {
	restore := stubMoveToNewRetryHooks(t)
	defer restore()

	attempts := 0
	moveQueuedJobToNewInstance = func(_ *sql.DB, jobID int64) (orchestration.Result, error) {
		attempts++
		if attempts == 1 {
			return orchestration.Result{}, cloud.ErrProviderRejected
		}
		return orchestration.Result{TargetDesc: "new A100 instance wi42"}, nil
	}

	err := moveJobsToNewInstances(nil, []*db.Job{{ID: 123}}, false, false)
	if err != nil {
		t.Fatalf("moveJobsToNewInstances: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestMoveJobsToNewInstancesRetriesBulkRetryableLaunch(t *testing.T) {
	restore := stubMoveToNewRetryHooks(t)
	defer restore()

	attempts := 0
	moveQueuedJobsToNewInstances = func(_ *sql.DB, _ []*db.Job, _ bool, _ orchestration.BulkCallbacks) (orchestration.BulkResult, error) {
		attempts++
		if attempts == 1 {
			return orchestration.BulkResult{}, cloud.ErrProviderRejected
		}
		return orchestration.BulkResult{InstanceIDs: []int64{42}}, nil
	}

	err := moveJobsToNewInstances(nil, []*db.Job{{ID: 123}, {ID: 124}}, false, false)
	if err != nil {
		t.Fatalf("moveJobsToNewInstances: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestMoveJobsToNewInstancesDoesNotRetryNonRetryableLaunch(t *testing.T) {
	restore := stubMoveToNewRetryHooks(t)
	defer restore()

	attempts := 0
	sentinel := errors.New("bad config")
	moveQueuedJobToNewInstance = func(_ *sql.DB, jobID int64) (orchestration.Result, error) {
		attempts++
		return orchestration.Result{}, sentinel
	}

	err := moveJobsToNewInstances(nil, []*db.Job{{ID: 123}}, false, false)
	if !errors.Is(err, sentinel) {
		t.Fatalf("moveJobsToNewInstances err = %v, want %v", err, sentinel)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
}

func TestShouldPrintMoveLaunchStatus(t *testing.T) {
	tests := []struct {
		name             string
		useLaunchTUI     bool
		launchTUIStarted bool
		want             bool
	}{
		{name: "plain mode before tui", want: true},
		{name: "plain mode after tui started", launchTUIStarted: true, want: false},
		{name: "tui mode before tui starts", useLaunchTUI: true, want: false},
		{name: "tui mode after tui starts", useLaunchTUI: true, launchTUIStarted: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldPrintMoveLaunchStatus(tt.useLaunchTUI, tt.launchTUIStarted); got != tt.want {
				t.Fatalf("shouldPrintMoveLaunchStatus() = %t, want %t", got, tt.want)
			}
		})
	}
}

func stubMoveToNewRetryHooks(t *testing.T) func() {
	t.Helper()
	origSingle := moveQueuedJobToNewInstance
	origBulk := moveQueuedJobsToNewInstances
	origMaxAttempts := moveNewMaxAttempts
	origBackoff := moveNewBackoffDelay
	origWait := moveNewWait
	moveNewMaxAttempts = func() int { return 3 }
	moveNewBackoffDelay = func(attempt int) (time.Duration, bool) { return 0, true }
	moveNewWait = func(context.Context, time.Duration) error { return nil }
	return func() {
		moveQueuedJobToNewInstance = origSingle
		moveQueuedJobsToNewInstances = origBulk
		moveNewMaxAttempts = origMaxAttempts
		moveNewBackoffDelay = origBackoff
		moveNewWait = origWait
	}
}
