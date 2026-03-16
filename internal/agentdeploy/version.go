package agentdeploy

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/osteele/weft/internal/ssh"
)

// RepoRoot returns the root directory of the weft source tree.
// First tries the CWD-based VCS root, validating it contains the weft go.mod.
// Falls back to the compile-time source directory if CWD is in a different repo.
func RepoRoot() (string, error) {
	// Try CWD-based VCS root first
	if root, err := jjWorkspaceRoot(); err == nil {
		if isWeftRoot(root) {
			return root, nil
		}
	}
	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		root := strings.TrimSpace(string(out))
		if isWeftRoot(root) {
			return root, nil
		}
	}

	// Fall back to compile-time source directory
	if root := compileTimeRoot(); root != "" && isWeftRoot(root) {
		return root, nil
	}

	return "", fmt.Errorf("weft source tree not found (CWD is in a different repo)")
}

// isWeftRoot checks if a directory is the weft source tree root
// by looking for a go.mod with the weft module path.
func isWeftRoot(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), "module github.com/osteele/weft")
}

// compileTimeRoot returns the source tree root based on the file path
// of this source file at compile time (via runtime.Caller).
func compileTimeRoot() string {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	// thisFile is .../internal/agentdeploy/version.go — go up 3 levels
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	return root
}

// LocalAgentVersion returns a stable version string for the agent binary.
// Uses the most recent commit that touched agent source files (cmd/agent/ or
// internal/), so unrelated file changes don't trigger a cross-compile.
// Uses RepoRoot() to find the weft source tree, so this works even when
// CWD is in a different repository.
func LocalAgentVersion() (string, error) {
	repoRoot, err := RepoRoot()
	if err != nil {
		return "", err
	}

	if version, err := jjVersion(repoRoot); err == nil {
		return version, nil
	}

	if version, err := gitVersion(repoRoot); err == nil {
		return version, nil
	}

	return "", fmt.Errorf("no VCS found in weft source tree %s", repoRoot)
}

// agentSourcePaths are the directories whose changes affect the agent binary.
var agentSourcePaths = []string{"cmd/agent/", "internal/"}

func jjVersion(repoRoot string) (string, error) {
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

func jjWorkspaceRoot() (string, error) {
	out, err := exec.Command("jj", "workspace", "root").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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

func gitVersion(repoRoot string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--short=12", "HEAD")
	cmd.Dir = repoRoot
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

// ErrAgentIncompatible is returned by RemoteAgentVersion when the agent binary
// exists on the remote host but fails to run (e.g. glibc version mismatch).
// Callers should treat this the same as "not installed" and redeploy.
var ErrAgentIncompatible = errors.New("agent binary incompatible")

// RemoteAgentVersion runs the agent binary on the remote host and parses
// the version string. Returns empty string if the agent is not installed.
// Returns ErrAgentIncompatible (wrapping the output) if the binary exists but
// fails to run (e.g. glibc mismatch) — callers should redeploy in that case.
func RemoteAgentVersion(host string) (string, error) {
	// Use a two-step check: first test if the binary exists, then run it.
	// This distinguishes "not installed" (return "") from "exists but crashes"
	// (return ErrAgentIncompatible), so callers can deploy the correct variant.
	checkCmd := fmt.Sprintf(
		`if [ ! -f %s ]; then echo "not-installed"; exit 0; fi; %s --version 2>&1`,
		remoteAgentPath, remoteAgentPath,
	)
	stdout, _, err := ssh.Run(host, checkCmd)
	if err != nil {
		// If this is an SSH connection error, we can't tell anything about the binary.
		if ssh.IsConnectionError(err.Error()) {
			return "", fmt.Errorf("remote agent version: %w", err)
		}
		// Non-zero exit: the binary exists but crashed (e.g. glibc mismatch).
		// stdout contains the crash output from 2>&1.
		output := strings.TrimSpace(stdout)
		return "", fmt.Errorf("%w on %s: %s", ErrAgentIncompatible, host, output)
	}
	output := strings.TrimSpace(stdout)
	if output == "not-installed" {
		return "", nil
	}
	ver := parseAgentVersionOutput(output)
	if ver == "" && output != "" {
		// Binary exists but didn't print a recognizable version — likely a
		// runtime error such as a glibc version mismatch.
		return "", fmt.Errorf("%w on %s: %s", ErrAgentIncompatible, host, output)
	}
	return ver, nil
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
