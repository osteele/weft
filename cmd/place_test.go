package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/orchestration"
)

func TestPlaceCommandsExposeMinSurvival(t *testing.T) {
	for _, args := range [][]string{
		{"place", "--help"},
		{"job", "place", "--help"},
	} {
		cmd, _, err := rootCmd.Find(args)
		if err != nil {
			t.Fatalf("find %v: %v", args, err)
		}
		if cmd == nil {
			t.Fatalf("find %v returned nil command", args)
		}
		if cmd.Flags().Lookup("min-survival") == nil {
			t.Fatalf("%q missing --min-survival", cmd.CommandPath())
		}
	}
}

func TestJobPlaceMinSurvivalFeedsLaunchOptions(t *testing.T) {
	oldMinSurvival := instanceLaunchMinSurvival
	defer func() { instanceLaunchMinSurvival = oldMinSurvival }()

	cmd, _, err := rootCmd.Find([]string{"job", "place", "--help"})
	if err != nil {
		t.Fatalf("find job place: %v", err)
	}
	if err := cmd.Flags().Set("min-survival", "0"); err != nil {
		t.Fatalf("set min-survival: %v", err)
	}

	opts := parseLaunchOpts()
	if opts.MinSurvival != 0 {
		t.Fatalf("MinSurvival = %v, want 0", opts.MinSurvival)
	}
}

func TestPlaceLeaseRunsWhileAutopilotPaused(t *testing.T) {
	database := db.SetupTestDB(t)
	if _, err := db.PauseAutopilot(database, "tester", "manual hold"); err != nil {
		t.Fatalf("PauseAutopilot: %v", err)
	}
	called := false
	if err := runPlaceWithAutopilotLease(nil, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("runPlaceWithAutopilotLease: %v", err)
	}
	if !called {
		t.Fatal("manual place body was not called")
	}
	paused, err := db.IsAutopilotPaused(database)
	if err != nil {
		t.Fatalf("IsAutopilotPaused: %v", err)
	}
	if !paused {
		t.Fatal("manual place must not clear sticky autopilot pause")
	}
}

func TestPlaceLeaseRejectsActiveAutopilotPass(t *testing.T) {
	database := db.SetupTestDB(t)
	runner := orchestration.NewAutopilotRunner(database, "test-autopilot")
	if err := runner.TryAcquire(); err != nil {
		t.Fatalf("seed autopilot acquire: %v", err)
	}
	defer runner.Release(0, "", nil)

	err := runPlaceWithAutopilotLease(nil, func() error {
		t.Fatal("manual place body should not run while autopilot owns the pass slot")
		return nil
	})
	if err == nil {
		t.Fatal("runPlaceWithAutopilotLease succeeded, want busy error")
	}
	if !strings.Contains(err.Error(), "autopilot is currently placing jobs") {
		t.Fatalf("error = %q, want autopilot busy message", err)
	}
}
