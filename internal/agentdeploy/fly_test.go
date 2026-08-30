package agentdeploy

import (
	"slices"
	"strings"
	"testing"
)

func TestFlyRemoteShellCommand_WrapsScriptInBashLoginCommand(t *testing.T) {
	script := "set -euo pipefail\nmkdir -p /data/weft-builder"
	cmd := flyRemoteShellCommand(script)
	if !strings.HasPrefix(cmd, "bash -lc ") {
		t.Fatalf("fly remote command must be executed via bash -lc, got: %q", cmd)
	}
	if !strings.Contains(cmd, "set -euo pipefail") {
		t.Fatalf("wrapped command does not include script content: %q", cmd)
	}
}

func TestFlyDownloadArgs_UsePortableRsyncProgress(t *testing.T) {
	args := flyDownloadArgs("rsh", "/remote/agent", "/local/agent")
	if !slices.Contains(args, "--progress") {
		t.Fatalf("download args must report progress, got: %q", args)
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--info=") {
			t.Fatalf("download args must work with Apple rsync, got GNU-only option: %q", arg)
		}
	}
}
