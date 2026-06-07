package queuerunner

import (
	"strings"
	"testing"

	"github.com/osteele/weft/internal/agentenv"
)

func TestRunnerCommandAddsUserToolPath(t *testing.T) {
	cmd := RunnerCommand("", "", 0)
	for _, want := range []string{
		agentenv.ShellPathAssignment(),
		`$HOME/.cache/weft/bin/weft-agent run-queue`,
	} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("RunnerCommand() = %q, missing %q", cmd, want)
		}
	}
}
