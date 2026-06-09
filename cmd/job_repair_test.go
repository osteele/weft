package cmd

import "testing"

func TestJobRepairCloudCompletionsReplacesStandaloneCommand(t *testing.T) {
	nested, _, err := jobCmd.Find([]string{"repair", "cloud-completions"})
	if err != nil {
		t.Fatalf("find nested repair command: %v", err)
	}
	if nested == nil || nested.Name() != "cloud-completions" {
		t.Fatalf("nested command = %v, want cloud-completions", nested)
	}

	standalone, _, err := jobCmd.Find([]string{"repair-cloud-completions"})
	if err != nil {
		t.Fatalf("find standalone repair command: %v", err)
	}
	if standalone != jobCmd {
		t.Fatalf("standalone repair-cloud-completions still resolves to %q", standalone.CommandPath())
	}
}
