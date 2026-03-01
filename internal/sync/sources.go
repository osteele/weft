package sync

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultExcludes returns the hardcoded rsync exclude patterns for source syncing.
// These cover common build artifacts, caches, and tool-specific directories.
func DefaultExcludes() []string {
	return []string{
		// VCS
		".git", ".jj",
		// Python environments and caches
		".venv", "venv", ".conda", ".direnv", ".pixi",
		"__pycache__", ".ruff_cache", ".pyright", ".uv", ".uv-cache",
		".mypy_cache", ".pytest_cache",
		"*.pyc",
		// Python build artifacts
		"*.egg-info", "*.egg", "*.whl", "dist",
		// macOS
		".DS_Store", "._*",
		// JS/TS
		"node_modules",
		// C/C++ build
		"build", "cmake-build-*", ".ccache", ".cmake",
		// Coverage and notebooks
		".coverage", "htmlcov", ".ipynb_checkpoints",
		// Generic build outputs
		"out", "target", "bin", "*.so", "cache", ".cache",
		// Editor and tool configs
		".vscode", ".claude", ".env",
		// AI/dev guidance files
		"CLAUDE.md", "AGENTS.md", "WARP.md",
	}
}

// BuildRsyncArgs constructs the rsync argument list for syncing sources to a remote host.
// host is the SSH hostname, localDir is an absolute local path (with trailing slash added),
// remoteDir may contain ~ (rsync expands it on the remote side).
func BuildRsyncArgs(host, localDir, remoteDir string, excludes []string) []string {
	args := []string{"-az", "--delete"}
	for _, pattern := range excludes {
		args = append(args, "--exclude", pattern)
	}
	// Ensure trailing slash so rsync syncs contents, not the directory itself
	src := strings.TrimRight(localDir, "/") + "/"
	dst := host + ":" + strings.TrimRight(remoteDir, "/") + "/"
	args = append(args, src, dst)
	return args
}

// SyncSources rsyncs localDir to host:remoteDir with standard excludes.
// localDir is an absolute local path. remoteDir may contain ~ (rsync expands it).
// Progress and timing are printed to stderr. Returns error on failure.
func SyncSources(host, localDir, remoteDir string) error {
	fmt.Fprintf(os.Stderr, "Syncing sources to %s:%s...\n", host, remoteDir)
	start := time.Now()

	args := BuildRsyncArgs(host, localDir, remoteDir, DefaultExcludes())
	cmd := exec.Command("rsync", args...)
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rsync to %s:%s: %w", host, remoteDir, err)
	}

	fmt.Fprintf(os.Stderr, "Synced (%.1fs)\n", time.Since(start).Seconds())
	return nil
}
