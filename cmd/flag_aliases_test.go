package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestJobAddFlagAliasesParse(t *testing.T) {
	var dir string
	var project string
	c := &cobra.Command{
		Use: "test",
		RunE: func(_ *cobra.Command, _ []string) error {
			return nil
		},
	}
	c.Flags().StringVarP(&dir, "directory", "C", "", "")
	c.Flags().StringVar(&project, "project", "", "")
	addJobAddFlagAliases(c)

	c.SetArgs([]string{
		"--dir", "/tmp/work",
		"--project", "exp-a",
	})
	if err := c.Execute(); err != nil {
		t.Fatalf("execute command: %v", err)
	}

	if dir != "/tmp/work" {
		t.Fatalf("directory = %q, want %q", dir, "/tmp/work")
	}
	if project != "exp-a" {
		t.Fatalf("project = %q, want %q", project, "exp-a")
	}
}

func TestJobAddFlagAliasesApplied(t *testing.T) {
	tests := []struct {
		name string
		cmd  *cobra.Command
	}{
		{name: "run", cmd: runCmd},
		{name: "queue add", cmd: queueAddCmd},
		{name: "job run", cmd: jobRunCmd},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dirFlag := tt.cmd.Flags().Lookup("dir")
			if dirFlag == nil {
				t.Fatalf("dir alias not found")
			}
			if dirFlag.Name != "directory" {
				t.Fatalf("dir alias normalized to %q, want %q", dirFlag.Name, "directory")
			}

			projectFlag := tt.cmd.Flags().Lookup("project")
			if projectFlag == nil {
				t.Fatalf("project flag not found")
			}
		})
	}
}
