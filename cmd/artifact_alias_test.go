package cmd

import "testing"

func TestArtifactListAliasesResolveToListCommand(t *testing.T) {
	for _, args := range [][]string{
		{"artifact", "ls", "wj1"},
		{"artifacts", "ls", "wj1"},
	} {
		cmd, remaining, err := rootCmd.Find(args)
		if err != nil {
			t.Fatalf("Find(%v): %v", args, err)
		}
		if cmd != artifactListCmd {
			t.Fatalf("Find(%v) command = %q, want artifactListCmd", args, cmd.CommandPath())
		}
		if len(remaining) != 1 || remaining[0] != "wj1" {
			t.Fatalf("Find(%v) remaining args = %v, want [wj1]", args, remaining)
		}
	}
}

func TestListAliasesResolveToListCommand(t *testing.T) {
	for _, args := range [][]string{
		{"ls"},
		{"ls", "artifacts", "wj1"},
	} {
		cmd, _, err := rootCmd.Find(args)
		if err != nil {
			t.Fatalf("Find(%v): %v", args, err)
		}
		if cmd != listCmd && cmd.Parent() != listCmd {
			t.Fatalf("Find(%v) command = %q, want listCmd or a list subcommand", args, cmd.CommandPath())
		}
	}
}

func TestArtifactPruneAliasesResolveToPruneCommand(t *testing.T) {
	for _, args := range [][]string{
		{"artifact", "prune", "--local"},
		{"artifacts", "prune", "--local"},
		{"artifact", "prune-local"},
		{"artifacts", "prune-local"},
	} {
		cmd, _, err := rootCmd.Find(args)
		if err != nil {
			t.Fatalf("Find(%v): %v", args, err)
		}
		if cmd != artifactPruneCmd {
			t.Fatalf("Find(%v) command = %q, want artifactPruneCmd", args, cmd.CommandPath())
		}
	}
}
