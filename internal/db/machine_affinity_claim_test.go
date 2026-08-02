package db

import (
	"database/sql"
	"errors"
	"testing"
)

// MachineRefKey has to normalize both stored forms, because the resolver keys
// wi/wj tokens to provider/machine_id but keeps a bare machine id exactly as the
// user typed it. An earlier fix compared only the keyed form, so
// `--affinity 49863` matched nothing — which, on a path that reads an empty
// affinity set as "unconstrained", silently became "place anywhere".
//
// A bare id means vast.ai, the only provider --affinity accepts unqualified.
// Both enforcement layers go through this function so they cannot disagree on
// what a stored ref names.
func TestMachineRefKey(t *testing.T) {
	cases := []struct {
		name              string
		ref               string
		provider, machine string
		want              bool
	}{
		{"bare id matches", "49863", "vastai", "49863", true},
		{"qualified ref matches", "vastai/49863", "vastai", "49863", true},
		{"bare id, wrong machine", "49863", "vastai", "140870", false},
		{"qualified ref, wrong machine", "vastai/49863", "vastai", "140870", false},
		{"qualified ref, wrong provider", "vastai/49863", "runpod", "49863", false},
		{"bare id does not match another provider", "49863", "runpod", "49863", false},
		{"empty ref", "", "vastai", "49863", false},
		{"no machine reported", "49863", "vastai", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key := MachineRefKey(tc.ref)
			got := key != "" && key == ProviderMachineKey(tc.provider, tc.machine)
			if got != tc.want {
				t.Errorf("MachineRefKey(%q)=%q vs ProviderMachineKey(%q, %q) matched = %v, want %v",
					tc.ref, key, tc.provider, tc.machine, got, tc.want)
			}
		})
	}
}

// The backstop. Every path that binds a job to a cloud launch goes through
// setJobLaunchIDOnce, so the invariant holds even when an upstream filter is
// missing.
func TestClaimRefusesPinnedJobOnWrongMachine(t *testing.T) {
	database := setupTestDB(t)
	jobID := pinnedTestJob(t, database, `{"machine_affinity":["49863"]}`)
	wrongLaunch := testLaunchOnMachine(t, database, "vastai", "140870")

	err := SetJobLaunchID(database, jobID, wrongLaunch)
	if err == nil {
		t.Fatal("claim succeeded for a job pinned to another machine")
	}
	if !errors.Is(err, ErrJobPinnedToOtherMachine) {
		t.Fatalf("err = %v, want ErrJobPinnedToOtherMachine", err)
	}
}

// The pin here is the bare form, which is what a user typing `--affinity 49863`
// stores; the provider-qualified form only arrives via a wi/wj token.
func TestClaimAllowsPinnedJobOnCorrectMachineFromBarePin(t *testing.T) {
	database := setupTestDB(t)
	jobID := pinnedTestJob(t, database, `{"machine_affinity":["49863"]}`)
	rightLaunch := testLaunchOnMachine(t, database, "vastai", "49863")

	if err := SetJobLaunchID(database, jobID, rightLaunch); err != nil {
		t.Fatalf("claim rejected the pinned machine: %v", err)
	}
}

// A launch reporting no machine is not confirmed to be the pinned one, so the
// claim fails closed.
func TestClaimRefusesPinnedJobWhenMachineUnknown(t *testing.T) {
	database := setupTestDB(t)
	jobID := pinnedTestJob(t, database, `{"machine_affinity":["49863"]}`)
	unknownLaunch := testLaunchOnMachine(t, database, "runpod", "")

	if err := SetJobLaunchID(database, jobID, unknownLaunch); !errors.Is(err, ErrJobPinnedToOtherMachine) {
		t.Fatalf("err = %v, want refusal when the launch reports no machine", err)
	}
}

// The backstop runs on every placement, so a false positive here would break
// all placement.
func TestClaimUnaffectedWithoutPin(t *testing.T) {
	for _, overrides := range []string{"", "{}", `{"gpu":"a100"}`} {
		database := setupTestDB(t)
		jobID := pinnedTestJob(t, database, overrides)
		launchID := testLaunchOnMachine(t, database, "vastai", "140870")
		if err := SetJobLaunchID(database, jobID, launchID); err != nil {
			t.Errorf("cli_overrides=%q: claim rejected an unpinned job: %v", overrides, err)
		}
	}
}

// Move-target attempts bind a job to its destination without passing
// setJobLaunchIDOnce, so CreateMoveTargetAttempt carries the same backstop —
// otherwise a move was the one claim write a pinned job could slip through.
func TestMoveTargetAttemptRefusesPinnedJobOnWrongMachine(t *testing.T) {
	database := setupTestDB(t)
	jobID := pinnedTestJob(t, database, `{"machine_affinity":["49863"]}`)
	wrongLaunch := testLaunchOnMachine(t, database, "vastai", "140870")
	intent := openMoveIntent(t, database, jobID)

	if _, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "", &wrongLaunch, StatusQueued); !errors.Is(err, ErrJobPinnedToOtherMachine) {
		t.Fatalf("err = %v, want ErrJobPinnedToOtherMachine", err)
	}
}

func TestMoveTargetAttemptAllowsPinnedJobOnItsMachine(t *testing.T) {
	database := setupTestDB(t)
	jobID := pinnedTestJob(t, database, `{"machine_affinity":["49863"]}`)
	rightLaunch := testLaunchOnMachine(t, database, "vastai", "49863")
	intent := openMoveIntent(t, database, jobID)

	if _, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "", &rightLaunch, StatusQueued); err != nil {
		t.Fatalf("move to the pinned machine rejected: %v", err)
	}
}

// A host destination can never satisfy a pin: a pin names a provider physical
// machine, which no on-prem host is.
func TestMoveTargetAttemptRefusesPinnedJobOnHostDestination(t *testing.T) {
	database := setupTestDB(t)
	jobID := pinnedTestJob(t, database, `{"machine_affinity":["49863"]}`)
	intent := openMoveIntent(t, database, jobID)

	if _, err := CreateMoveTargetAttempt(database, intent.ID, jobID, "host-alpha", nil, StatusQueued); !errors.Is(err, ErrJobPinnedToOtherMachine) {
		t.Fatalf("err = %v, want ErrJobPinnedToOtherMachine for an on-prem move destination", err)
	}
}

func openMoveIntent(t *testing.T, database *sql.DB, jobID int64) *MoveIntent {
	t.Helper()
	intent, err := CreateMoveIntent(database, CreateMoveIntentParams{JobID: jobID, TargetKind: MoveTargetNew})
	if err != nil {
		t.Fatalf("CreateMoveIntent: %v", err)
	}
	return intent
}

// A pin names a provider physical machine, which no on-prem host is. The
// jobs.host write is the on-prem counterpart of the cloud claim boundary, so
// it carries the same backstop: eligibility already rejects pinned jobs on
// every host, and this survives an upstream filter that goes missing.
func TestAssignJobHostRefusesPinnedJob(t *testing.T) {
	database := setupTestDB(t)
	jobID := pinnedTestJob(t, database, `{"machine_affinity":["49863"]}`)

	assigned, err := AssignJobHost(database, jobID, "host-alpha")
	if !errors.Is(err, ErrJobPinnedToOtherMachine) {
		t.Fatalf("err = %v, want ErrJobPinnedToOtherMachine", err)
	}
	if assigned {
		t.Fatal("AssignJobHost bound a pinned job to an on-prem host")
	}
}

func TestAssignJobHostUnaffectedWithoutPin(t *testing.T) {
	for _, overrides := range []string{"", "{}", `{"gpu":"a100"}`} {
		database := setupTestDB(t)
		jobID := pinnedTestJob(t, database, overrides)
		assigned, err := AssignJobHost(database, jobID, "host-alpha")
		if err != nil || !assigned {
			t.Errorf("cli_overrides=%q: AssignJobHost = (%v, %v), want unpinned job assigned", overrides, assigned, err)
		}
	}
}

func pinnedTestJob(t *testing.T, database *sql.DB, overridesJSON string) int64 {
	t.Helper()
	jobID, err := RecordQueuedWithGPU(database, "", "/tmp/project", "python probe.py", "cloud", "")
	if err != nil {
		t.Fatalf("RecordQueuedWithGPU: %v", err)
	}
	if overridesJSON != "" {
		if _, err := database.Exec(`UPDATE jobs SET cli_overrides = ? WHERE id = ?`, overridesJSON, jobID); err != nil {
			t.Fatalf("set cli_overrides: %v", err)
		}
	}
	return jobID
}

func testLaunchOnMachine(t *testing.T, database *sql.DB, provider, machineID string) int64 {
	t.Helper()
	launchID, err := CreateLaunch(database, &Launch{
		Status:    LaunchStatusRunning,
		Provider:  provider,
		MachineID: machineID,
		GPUSpec:   "RTX_4090",
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	return launchID
}

// driver_version roundtrips through CreateLaunch and the shared scan so reuse
// admission can read the fact back; empty stays empty (unknown, not "0").
func TestLaunchDriverVersionRoundtrip(t *testing.T) {
	database := setupTestDB(t)
	for _, version := range []string{"550.90.07", ""} {
		id, err := CreateLaunch(database, &Launch{
			Status: LaunchStatusRunning, Provider: "vastai",
			GPUSpec: "RTX_4090", DriverVersion: version,
		})
		if err != nil {
			t.Fatalf("CreateLaunch: %v", err)
		}
		launch, err := GetLaunch(database, id)
		if err != nil {
			t.Fatalf("GetLaunch: %v", err)
		}
		if launch.DriverVersion != version {
			t.Errorf("DriverVersion = %q, want %q", launch.DriverVersion, version)
		}
	}

	id, err := CreateLaunch(database, &Launch{Status: LaunchStatusRunning, Provider: "runpod", GPUSpec: "H100"})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	if err := UpdateLaunchDriverVersion(database, id, " 550.127.05\n"); err != nil {
		t.Fatalf("UpdateLaunchDriverVersion: %v", err)
	}
	launch, err := GetLaunch(database, id)
	if err != nil {
		t.Fatalf("GetLaunch: %v", err)
	}
	if launch.DriverVersion != "550.127.05" {
		t.Errorf("DriverVersion = %q, want probe write-back trimmed", launch.DriverVersion)
	}
}
