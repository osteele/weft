package cmd

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/db"
)

// The hint must stay silent unless weft can actually show the declared floor
// was met. An absent observation is not evidence either way, and a wrong hint
// is worse than none — the whole defect being fixed is a misleading adjacency.
func TestDriverFloorSatisfiedHint_SilentWithoutEvidence(t *testing.T) {
	database := db.SetupTestDB(t)
	mkJob := func(reason string) *db.Job {
		return &db.Job{
			FailureReason: reason,
			GPUClass:      "nvidia",
			CLIResourceOverrides: &db.CLIResourceOverrides{
				MinCUDAVersion: "12.4",
			},
		}
	}
	for name, job := range map[string]*db.Job{
		"nil job":            nil,
		"no launch observed": mkJob(db.FailureReasonCUDADriverTooOld),
		"unrelated failure":  mkJob("some_other_reason"),
	} {
		t.Run(name, func(t *testing.T) {
			if hint := driverFloorSatisfiedHint(database, job); hint != "" {
				t.Errorf("hint emitted without evidence: %q", hint)
			}
		})
	}
}

// The hint fires when the machine's observed driver met the declared floor:
// that is exactly the case where the adjacent "Driver floor" line misleads.
func TestDriverFloorSatisfiedHint_NamesTheResolvedRuntime(t *testing.T) {
	database := db.SetupTestDB(t)
	launchID, err := db.CreateLaunch(database, &db.Launch{
		Status:        db.LaunchStatusFailed,
		DriverVersion: "550.90.07",
		CUDAVersion:   12.8,
	})
	if err != nil {
		t.Fatalf("CreateLaunch: %v", err)
	}
	job := &db.Job{
		FailureReason:        db.FailureReasonCUDADriverTooOld,
		GPUClass:             "nvidia",
		LaunchID:             &launchID,
		CLIResourceOverrides: &db.CLIResourceOverrides{MinCUDAVersion: "12.4"},
	}
	hint := driverFloorSatisfiedHint(database, job)
	if hint == "" {
		t.Fatal("no hint for a driver-too-old failure on a machine that met the floor")
	}
	for _, want := range []string{"550.90.07", "met this job's declared floor", "not the cause"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q:\n%s", want, hint)
		}
	}
	// An older machine genuinely below the floor must NOT get the hint — there
	// the declaration really is the story.
	if err := db.UpdateLaunchDriverVersion(database, launchID, "470.10.01"); err != nil {
		t.Fatalf("UpdateLaunchDriverVersion: %v", err)
	}
	if hint := driverFloorSatisfiedHint(database, job); hint != "" {
		t.Errorf("hint emitted for a machine below the floor: %q", hint)
	}
}
