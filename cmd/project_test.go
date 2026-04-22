package cmd

import (
	"os"
	"strings"
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

func TestErrNoJobsForProjectWording(t *testing.T) {
	tmpfile, err := os.CreateTemp("", "errnojobs-*.db")
	if err != nil {
		t.Fatalf("create temp db: %v", err)
	}
	tmpfile.Close()
	defer os.Remove(tmpfile.Name())

	cleanup := db.SetDBPath(tmpfile.Name())
	defer cleanup()

	database, err := db.Open()
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer database.Close()

	jobID, err := db.RecordJobStarting(database, "test-host", "/home/user/alpha", "echo hi", "")
	if err != nil {
		t.Fatalf("record job: %v", err)
	}
	if err := db.SetJobProject(database, jobID, "alpha"); err != nil {
		t.Fatalf("SetJobProject: %v", err)
	}

	// Project with jobs but filters excluded all of them: no cwd hint.
	msg := errNoJobsForProject(database, "alpha").Error()
	if !strings.Contains(msg, "matching the current filters") {
		t.Errorf("message for known project missing filters clause: %q", msg)
	}
	if strings.Contains(msg, "is the current directory") {
		t.Errorf("message for known project should not mention current directory: %q", msg)
	}

	// Project with no jobs at all: keep the cwd hint.
	msg = errNoJobsForProject(database, "nonexistent").Error()
	if !strings.Contains(msg, "is the current directory a known project?") {
		t.Errorf("message for unknown project missing cwd hint: %q", msg)
	}
}
