package db

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestLaunchStateMachine_RandomCommands exercises the launch lifecycle helpers
// with randomly generated command sequences. It does not model the exact
// transition graph (the implementation is intentionally permissive about some
// non-terminal transitions); instead it checks the invariants declared in
// specs/campaign-lifecycle.allium:
//   - terminal statuses are sticky and always have ended_at and termination_reason
//   - grace status implies grace_started_at and grace_deadline are set
//   - provider_running_at is first-write-wins
//
// The test logs the random seed on failure; set WEFT_TEST_SEED to reproduce.
func TestLaunchStateMachine_RandomCommands(t *testing.T) {
	seed := launchSMSeed()
	rng := rand.New(rand.NewPCG(seed, 0))
	t.Logf("launch state-machine seed: %d (set WEFT_TEST_SEED to reproduce)", seed)

	iterations := 50
	stepsPerRun := 30
	statuses := []string{
		LaunchStatusPlanned,
		LaunchStatusLaunching,
		LaunchStatusRunning,
		LaunchStatusPaused,
		LaunchStatusGrace,
		LaunchStatusCompleted,
		LaunchStatusFailed,
		LaunchStatusCancelled,
	}

	for i := 0; i < iterations; i++ {
		database := SetupTestDB(t)
		id, err := CreateLaunch(database, &Launch{
			Status:   LaunchStatusPlanned,
			Provider: "vastai",
			GPUSpec:  "RTX_4090",
		})
		if err != nil {
			t.Fatalf("iteration %d: CreateLaunch: %v", i, err)
		}

		var everTerminal bool
		var firstProviderRunningAt *int64

		for step := 0; step < stepsPerRun; step++ {
			cmd := randomLaunchCommand(rng, statuses)
			desc := cmd.describe()

			// Many commands return ErrLaunchTerminal when the launch has already
			// ended. That is expected; we only care that the persisted state
			// remains invariant.
			_ = cmd.run(database, id)

			launch, err := GetLaunch(database, id)
			if err != nil {
				t.Fatalf("iteration %d step %d: GetLaunch: %v\nseed=%d command=%s", i, step, err, seed, desc)
			}
			if launch == nil {
				t.Fatalf("iteration %d step %d: launch %d missing\nseed=%d command=%s", i, step, id, seed, desc)
			}

			isTerm := IsTerminalLaunchStatus(launch.Status)
			if isTerm {
				everTerminal = true
				if launch.EndedAt == nil || *launch.EndedAt <= 0 {
					t.Fatalf("iteration %d step %d: terminal status %q without ended_at\nseed=%d command=%s",
						i, step, launch.Status, seed, desc)
				}
				if launch.TerminationReason == "" {
					t.Fatalf("iteration %d step %d: terminal status %q without termination_reason\nseed=%d command=%s",
						i, step, launch.Status, seed, desc)
				}
			}
			if everTerminal && !isTerm {
				t.Fatalf("iteration %d step %d: launch was terminal but revived to %q\nseed=%d command=%s",
					i, step, launch.Status, seed, desc)
			}

			if launch.Status == LaunchStatusGrace {
				if launch.GraceStartedAt == nil || launch.GraceDeadline == nil {
					t.Fatalf("iteration %d step %d: grace status without grace timestamps\nseed=%d command=%s",
						i, step, seed, desc)
				}
			}

			if launch.ProviderRunningAt != nil {
				if firstProviderRunningAt == nil {
					firstProviderRunningAt = launch.ProviderRunningAt
				} else if *firstProviderRunningAt != *launch.ProviderRunningAt {
					t.Fatalf("iteration %d step %d: provider_running_at changed from %d to %d\nseed=%d command=%s",
						i, step, *firstProviderRunningAt, *launch.ProviderRunningAt, seed, desc)
				}
			}
		}
	}
}

// launchCommand is one mutating operation against a launch.
type launchCommand struct {
	name string
	args []any
	run  func(*sql.DB, int64) error
}

func (c launchCommand) describe() string {
	return fmt.Sprintf("%s%v", c.name, c.args)
}

func randomLaunchCommand(rng *rand.Rand, statuses []string) launchCommand {
	switch rng.IntN(6) {
	case 0:
		return randomUpdateStatusCommand(rng, statuses)
	case 1:
		deadline := time.Now().Add(time.Duration(rng.IntN(600)+1) * time.Second).Unix()
		return launchCommand{
			name: "SetLaunchGraceStarted",
			args: []any{deadline},
			run: func(db *sql.DB, id int64) error {
				return SetLaunchGraceStarted(db, id, deadline)
			},
		}
	case 2:
		return launchCommand{
			name: "ClearLaunchGrace",
			run:  ClearLaunchGrace,
		}
	case 3:
		deadline := time.Now().Add(time.Duration(rng.IntN(600)+1) * time.Second).Unix()
		return launchCommand{
			name: "ExtendLaunchGrace",
			args: []any{deadline},
			run: func(db *sql.DB, id int64) error {
				return ExtendLaunchGrace(db, id, deadline)
			},
		}
	case 4:
		ts := time.Now().Add(-time.Duration(rng.IntN(300)) * time.Second)
		return launchCommand{
			name: "SetLaunchProviderRunningAt",
			args: []any{ts.Unix()},
			run: func(db *sql.DB, id int64) error {
				return SetLaunchProviderRunningAt(db, id, ts)
			},
		}
	default:
		seconds := rng.IntN(3600) + 1
		return launchCommand{
			name: "SetLaunchGracePeriod",
			args: []any{seconds},
			run: func(db *sql.DB, id int64) error {
				return SetLaunchGracePeriod(db, id, seconds)
			},
		}
	}
}

func randomUpdateStatusCommand(rng *rand.Rand, statuses []string) launchCommand {
	to := statuses[rng.IntN(len(statuses))]
	reason := ""
	switch to {
	case LaunchStatusCompleted:
		reason = TerminationReasonCompleted
	case LaunchStatusCancelled:
		reason = TerminationReasonCancelled
	case LaunchStatusFailed:
		reason = randomFailureReason(rng)
	}

	if reason == "" {
		return launchCommand{
			name: "UpdateLaunchStatus",
			args: []any{to},
			run: func(db *sql.DB, id int64) error {
				return UpdateLaunchStatus(db, id, to)
			},
		}
	}
	return launchCommand{
		name: "UpdateLaunchStatus",
		args: []any{to, reason},
		run: func(db *sql.DB, id int64) error {
			return UpdateLaunchStatus(db, id, to, reason)
		},
	}
}

func randomFailureReason(rng *rand.Rand) string {
	reasons := []string{
		TerminationReasonProviderFailure,
		TerminationReasonJobFailure,
		TerminationReasonDiskFull,
		TerminationReasonInfraFailure,
		TerminationReasonBootstrapTimeout,
		TerminationReasonPhaseStall,
		TerminationReasonPreempted,
		TerminationReasonUnknown,
		TerminationReasonWeftBug,
		TerminationReasonProviderTimeout,
		TerminationReasonUploadStall,
		TerminationReasonAccountCreditExhausted,
	}
	return reasons[rng.IntN(len(reasons))]
}

func launchSMSeed() uint64 {
	if s := os.Getenv("WEFT_TEST_SEED"); s != "" {
		if n, err := strconv.ParseUint(s, 10, 64); err == nil {
			return n
		}
	}
	return rand.Uint64()
}
