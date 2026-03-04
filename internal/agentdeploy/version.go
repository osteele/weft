package agentdeploy

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/osteele/weft/internal/ssh"
)

// LocalAgentVersion returns a stable version string for the agent binary.
// Uses the most recent commit that touched agent source files (cmd/agent/ or
// internal/), so unrelated file changes don't trigger a cross-compile.
// Prefers jj, falls back to git HEAD if jj is unavailable.
func LocalAgentVersion() (string, error) {
	if version, err := jjVersion(); err == nil {
		return version, nil
	}

	if version, err := gitVersion(); err == nil {
		return version, nil
	}

	return "", fmt.Errorf("neither jj nor git repository found")
}

// agentSourcePaths are the directories whose changes affect the agent binary.
var agentSourcePaths = []string{"cmd/agent/", "internal/"}

func jjVersion() (string, error) {
	rootCmd := exec.Command("jj", "workspace", "root")
	rootOut, err := rootCmd.Output()
	if err != nil {
		return "", err
	}
	repoRoot := strings.TrimSpace(string(rootOut))

	// Find the most recent committed ancestor that touched agent source files.
	// Use @- (parent of working copy) to exclude the working copy itself,
	// because the working copy gets a new commit_id on every jj snapshot,
	// which would cause perpetual version mismatches and agent redeploys.
	args := []string{
		"log", "--no-graph",
		"-r", "ancestors(@-, 200)",
		"-T", `commit_id.short(12)`,
		"--limit", "1",
	}
	args = append(args, agentSourcePaths...)
	if version, err := jjLog(repoRoot, args...); err == nil && version != "" {
		return version, nil
	}

	// Fallback: no ancestor touched agent paths (e.g., brand new repo).
	return jjLog(repoRoot, "log", "--no-graph", "-r", "@-", "-T", `commit_id.short(12)`)
}

// jjLog runs a jj command in repoRoot and returns the trimmed output.
func jjLog(repoRoot string, args ...string) (string, error) {
	cmd := exec.Command("jj", args...)
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
