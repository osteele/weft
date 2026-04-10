package agentdeploy

import (
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
