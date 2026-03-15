package cmd

import "testing"

func TestCloudWatchCommandsRegistered(t *testing.T) {
	cmd, _, err := rootCmd.Find([]string{"cloud", "watch"})
	if err != nil {
		t.Fatalf("find cloud watch: %v", err)
	}
	if cmd != cloudWatchCmd {
		t.Fatalf("cloud watch resolved to %q, want cloudWatchCmd", cmd.Name())
	}

	cmd, _, err = rootCmd.Find([]string{"watch"})
	if err != nil {
		t.Fatalf("find root watch: %v", err)
	}
	if cmd != watchCmd {
		t.Fatalf("root watch resolved to %q, want watchCmd", cmd.Name())
	}
}
