package cmd

import (
	"context"
	"database/sql"
	"testing"

	"github.com/osteele/weft/internal/db"
	"github.com/spf13/cobra"
)

func TestJobTagCommandsAcceptJobAndTag(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{name: "add", cmd: jobTagAddCmd},
		{name: "rm", cmd: jobTagRemoveCmd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cmd.Args(tc.cmd, []string{"wj1906", "interruptible"}); err != nil {
				t.Fatalf("Args returned error for job+tag: %v", err)
			}
		})
	}
}

func TestSetJobTagSingleWriterUsesMutationLayer(t *testing.T) {
	database := db.SetupTestDB(t)
	orig := setJobTagMutationFunc
	t.Cleanup(func() { setJobTagMutationFunc = orig })
	var gotJobID int64
	var gotTag string
	var gotPresent bool
	setJobTagMutationFunc = func(_ context.Context, _ *sql.DB, jobID int64, tag string, present bool) error {
		gotJobID = jobID
		gotTag = tag
		gotPresent = present
		return nil
	}

	if err := setJobTagSingleWriter(database, 42, "interruptible", true); err != nil {
		t.Fatalf("setJobTagSingleWriter: %v", err)
	}
	if gotJobID != 42 || gotTag != "interruptible" || !gotPresent {
		t.Fatalf("mutation call = job %d tag %q present %v, want job 42 tag interruptible present true", gotJobID, gotTag, gotPresent)
	}
}
