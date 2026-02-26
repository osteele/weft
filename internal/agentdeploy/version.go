package agentdeploy

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/osteele/weft/internal/ssh"
)

// LocalAgentVersion returns the jj commit hash of the latest change
// touching agent-relevant source files (cmd/agent/ or internal/).
func LocalAgentVersion() (string, error) {
	cmd := exec.Command("jj", "log",
		"--no-graph",
		"-r", "ancestors(@, 50)",
		"-T", `commit_id.short(12) ++ "\n"`,
		"--limit", "1",
		"cmd/agent/", "internal/",
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("jj log: %w", err)
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", fmt.Errorf("no commits found touching agent sources")
	}
	return version, nil
}

const remoteAgentPath = "~/.cache/weft/bin/weft-agent"

// RemoteAgentVersion runs the agent binary on the remote host and parses
// the version string. Returns empty string if the agent is not installed.
func RemoteAgentVersion(host string) (string, error) {
	cmd := fmt.Sprintf("%s --version 2>/dev/null || true", remoteAgentPath)
	stdout, _, err := ssh.Run(host, cmd)
	if err != nil {
		return "", fmt.Errorf("remote agent version: %w", err)
	}
	return parseAgentVersionOutput(strings.TrimSpace(stdout)), nil
}

// parseAgentVersionOutput extracts the version from "weft-agent <version>" output.
// Returns empty string if the output doesn't match the expected format.
func parseAgentVersionOutput(output string) string {
	if output == "" {
		return ""
	}
	parts := strings.Fields(output)
	if len(parts) >= 2 && parts[0] == "weft-agent" {
		return parts[1]
	}
	return ""
}
