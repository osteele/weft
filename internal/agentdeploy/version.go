package agentdeploy

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/osteele/weft/internal/ssh"
)

// LocalAgentVersion returns the commit hash of the working copy.
// Prefers jj (which includes uncommitted changes in the working copy commit),
// falls back to git HEAD if jj is unavailable.
func LocalAgentVersion() (string, error) {
	// Try jj first — the working copy always has a commit, even with dirty files
	if version, err := jjVersion(); err == nil {
		return version, nil
	}

	// Fall back to git HEAD
	if version, err := gitVersion(); err == nil {
		return version, nil
	}

	return "", fmt.Errorf("neither jj nor git repository found")
}

func jjVersion() (string, error) {
	rootCmd := exec.Command("jj", "workspace", "root")
	rootOut, err := rootCmd.Output()
	if err != nil {
		return "", err
	}
	repoRoot := strings.TrimSpace(string(rootOut))

	cmd := exec.Command("jj", "log",
		"--no-graph",
		"-r", "@",
		"-T", `commit_id.short(12)`,
	)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", fmt.Errorf("empty jj commit id")
	}
	return version, nil
}

func gitVersion() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--short=12", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", fmt.Errorf("empty git commit id")
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
