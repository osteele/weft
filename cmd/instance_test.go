package cmd

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/retrypolicy"
)

func TestMarkReleasedInstanceFailed_ClosesUnresolvedJobsAsFailed(t *testing.T) {
	database := db.SetupTestDB(t)

	now := time.Now().Unix()
	graceDeadline := now + 300
	instanceID, err := db.CreateLaunch(database, &db.Launch{
		Status:         db.LaunchStatusGrace,
		Provider:       "vastai",
		GraceStartedAt: &now,
		GraceDeadline:  &graceDeadline,
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	jobID, err := db.RecordQueuedWithGPU(database, "", "/tmp", "echo test", "test", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobLaunchID(database, jobID, instanceID); err != nil {
		t.Fatalf("set cloud instance: %v", err)
	}
	if _, err := database.Exec(`UPDATE job_attempts SET status = ? WHERE job_id = ? AND end_time IS NULL`, db.StatusRunning, jobID); err != nil {
		t.Fatalf("set job running: %v", err)
	}

	if err := markReleasedInstanceFailed(database, instanceID); err != nil {
		t.Fatalf("markReleasedInstanceFailed: %v", err)
	}

	ci, err := db.GetLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get instance: %v", err)
	}
	if ci.Status != db.LaunchStatusFailed {
		t.Fatalf("instance status = %q, want %q", ci.Status, db.LaunchStatusFailed)
	}
	if ci.TerminationReason != db.TerminationReasonJobFailure {
		t.Fatalf("termination reason = %q, want %q", ci.TerminationReason, db.TerminationReasonJobFailure)
	}

	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if job.Status != db.StatusFailed {
		t.Fatalf("job status = %q, want %q", job.Status, db.StatusFailed)
	}
	if job.LaunchID == nil || *job.LaunchID != instanceID {
		t.Fatalf("job launch_id = %v, want %d", job.LaunchID, instanceID)
	}

	outcomes, err := db.GetAttemptOutcomesByLaunch(database, instanceID)
	if err != nil {
		t.Fatalf("get outcomes: %v", err)
	}
	if outcomes[jobID] != db.AttemptOutcomeFailed {
		t.Fatalf("attempt outcome = %q, want %q", outcomes[jobID], db.AttemptOutcomeFailed)
	}
}

func TestJoinNonEmpty(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"all empty", []string{"", "", ""}, ""},
		{"middle empty", []string{"a", "", "b"}, "a | b"},
		{"trailing empty", []string{"a", "b", ""}, "a | b"},
		{"leading empty", []string{"", "a", "b"}, "a | b"},
		{"all populated", []string{"a", "b", "c"}, "a | b | c"},
		{"whitespace treated as empty", []string{" ", "\t", "x"}, "x"},
		{"trims each segment", []string{"  a  ", "b\n"}, "a | b"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := joinNonEmpty(tc.in, " | ")
			if got != tc.want {
				t.Fatalf("joinNonEmpty(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestInstanceInfoCommandAliasExists(t *testing.T) {
	var found bool
	for _, sub := range instanceCmd.Commands() {
		if sub.Name() == "info" {
			found = true
			if sub.RunE == nil {
				t.Fatal("instance info command should have a handler")
			}
		}
	}
	if !found {
		t.Fatal("instance info command not found")
	}
}

func TestInstanceInfoStatusLabelShowsProviderTerminalState(t *testing.T) {
	ci := &db.Launch{Status: db.LaunchStatusRunning}
	inst := &cloud.Instance{Status: cloud.ProviderStatusExited}
	got := instanceInfoStatusLabel(ci, inst)
	if got != "running (exited)" {
		t.Fatalf("instanceInfoStatusLabel = %q, want running (exited)", got)
	}
}

func TestInstanceInfoStatusLabelOmitsProviderStateForTerminalLaunch(t *testing.T) {
	ci := &db.Launch{Status: db.LaunchStatusFailed}
	inst := &cloud.Instance{Status: cloud.ProviderStatusDestroyed}
	got := instanceInfoStatusLabel(ci, inst)
	if got != "terminated" {
		t.Fatalf("instanceInfoStatusLabel = %q, want terminated", got)
	}
}

func TestShouldRefreshInstanceLiveStateDefaultIsCached(t *testing.T) {
	ci := &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", ProviderInstanceID: "123"}
	if shouldRefreshInstanceLiveState(nil, ci, false, false) {
		t.Fatal("default instance status/info should use cached DB state")
	}
}

func TestShouldRefreshInstanceLiveStateSyncIsExplicit(t *testing.T) {
	ci := &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", ProviderInstanceID: "123"}
	if !shouldRefreshInstanceLiveState(nil, ci, true, false) {
		t.Fatal("--sync should request live provider/R2 refresh")
	}
}

func TestShouldRefreshInstanceLiveStateNoSyncWins(t *testing.T) {
	ci := &db.Launch{Status: db.LaunchStatusRunning, Provider: "vastai", ProviderInstanceID: "123"}
	if shouldRefreshInstanceLiveState(nil, ci, true, true) {
		t.Fatal("--no-sync should suppress live provider/R2 refresh")
	}
}

func TestShouldRefreshInstanceLiveStateTerminalIsCached(t *testing.T) {
	ci := &db.Launch{Status: db.LaunchStatusFailed, Provider: "vastai", ProviderInstanceID: "123"}
	if shouldRefreshInstanceLiveState(nil, ci, true, false) {
		t.Fatal("terminal instance info should not live-refresh")
	}
}

func TestFormatRentalLineShowsRescueCap(t *testing.T) {
	bid := 47
	cap := 57
	got := formatRentalLine(&db.Launch{
		InstanceType:     cloud.InstanceTypeInterruptible,
		MaxBidPriceCents: &bid,
		OnDemandRefCents: &cap,
	})
	want := "interruptible · max bid $0.47/hr · rescue cap $0.57/hr"
	if got != want {
		t.Fatalf("formatRentalLine = %q, want %q", got, want)
	}
}

func TestInstanceNewCommandExists(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"instance", "new", "--help"})
	if err != nil {
		t.Fatalf("find instance new: %v", err)
	}
	if cmd == nil || cmd.Name() != "new" {
		t.Fatalf("command = %v, want instance new", cmd)
	}
	for _, flag := range []string{"jobs", "project", "strategy", "min-survival", "dry-run", "yes", "wait", "timeout"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Fatalf("instance new missing --%s", flag)
		}
	}
}

func TestNewInstanceVerbAliasExists(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"new", "instance", "--help"})
	if err != nil {
		t.Fatalf("find new instance: %v", err)
	}
	if cmd == nil || cmd.Name() != "instance" {
		t.Fatalf("command = %v, want new instance alias", cmd)
	}
	if cmd.Flags().Lookup("project") == nil {
		t.Fatal("new instance alias missing --project")
	}
}

func TestConfirmInstanceNewLaunch(t *testing.T) {
	res := orchestration.NewInstanceResult{
		AnchorJobID: 42,
		JobIDs:      []int64{42, 43},
		Offer: &cloud.Offer{
			Provider:    cloud.ProviderVastai,
			GPUName:     "RTX 4090",
			CostPerHour: 0.95,
		},
	}

	var out bytes.Buffer
	ok, err := confirmInstanceNewLaunch(strings.NewReader("yes\n"), &out, res)
	if err != nil {
		t.Fatalf("confirmInstanceNewLaunch: %v", err)
	}
	if !ok {
		t.Fatal("confirmation should accept yes")
	}
	if !strings.Contains(out.String(), "Selected one instance for anchor wj42 with 2 job(s): wj42:wj43") {
		t.Fatalf("confirmation preview missing job summary: %q", out.String())
	}

	out.Reset()
	ok, err = confirmInstanceNewLaunch(strings.NewReader("\n"), &out, res)
	if err != nil {
		t.Fatalf("confirmInstanceNewLaunch: %v", err)
	}
	if ok {
		t.Fatal("empty confirmation should decline")
	}
}

func TestInstanceNewMaxAttempts_YesUsesRetryPolicy(t *testing.T) {
	oldYes, oldDryRun := instanceNewYes, instanceNewDryRun
	t.Cleanup(func() {
		instanceNewYes = oldYes
		instanceNewDryRun = oldDryRun
	})

	instanceNewYes = true
	instanceNewDryRun = false
	if got, want := instanceNewMaxAttempts(), retrypolicy.MaxAttempts(); got != want {
		t.Fatalf("instanceNewMaxAttempts() = %d, want %d", got, want)
	}

	instanceNewDryRun = true
	if got := instanceNewMaxAttempts(); got != 1 {
		t.Fatalf("dry-run instanceNewMaxAttempts() = %d, want 1", got)
	}
}
