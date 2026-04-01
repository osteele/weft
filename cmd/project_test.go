package cmd

import (
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

func TestFilterJobsByFailureState(t *testing.T) {
	jobs := []*db.Job{
		{ID: 1, Status: db.StatusCompleted, ExitCode: testIntPtr(0)},
		{ID: 2, Status: db.StatusCompleted, ExitCode: testIntPtr(3)},
		{ID: 3, Status: db.StatusFailed},
		{ID: 4, Status: db.StatusDead},
		{ID: 5, Status: db.StatusKilled},
	}

	filtered := filterJobsByFailureState(jobs, true)
	if len(filtered) != 3 {
		t.Fatalf("expected 3 failed jobs, got %d", len(filtered))
	}
	if filtered[0].ID != 2 || filtered[1].ID != 3 || filtered[2].ID != 4 {
		t.Fatalf("unexpected failed job IDs: %+v", filtered)
	}
}

func TestProjectCommandsExposeSharedListFlags(t *testing.T) {
	for _, cmd := range []*cobra.Command{jobListCmd, projectJobsCmd} {
		for _, name := range []string{"failed", "processed", "unprocessed", "rental", "inventory", "cloud"} {
			if flag := cmd.Flags().Lookup(name); flag == nil {
				t.Fatalf("%s missing flag %q", cmd.Name(), name)
			}
		}
		if flag := cmd.Flags().Lookup("cloud"); flag != nil && !flag.Hidden {
			t.Fatalf("%s cloud alias flag should be hidden", cmd.Name())
		}
	}
}

func testIntPtr(v int) *int {
	return &v
}
