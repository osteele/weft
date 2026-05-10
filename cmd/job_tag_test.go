package cmd

import (
	"testing"

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
