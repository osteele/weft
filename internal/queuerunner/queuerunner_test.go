package queuerunner

import (
	"strings"
	"testing"
)

func TestRunnerCommandAddsUserToolPath(t *testing.T) {
	cmd := RunnerCommand("", "", 0)
	for _, want := range []string{
		`PATH="$HOME/.cache/weft/bin:$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/local/sbin:$PATH"`,
		`$HOME/.cache/weft/bin/weft-agent run-queue`,
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("RunnerCommand() = %q, missing %q", cmd, want)
		}
	}
}
