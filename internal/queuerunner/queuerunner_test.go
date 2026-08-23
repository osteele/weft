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

func TestRunnerCommandForHostEnablesR2Queue(t *testing.T) {
	cmd := RunnerCommandForHost("", "jobs", "studio", 0)
	for _, want := range []string{"--r2-bucket=jobs", "--r2-queue-host=studio"} {
		if !strings.Contains(cmd, want) {
			t.Fatalf("RunnerCommandForHost() = %q, missing %q", cmd, want)
		}
	}
}
