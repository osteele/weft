package sync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
)

// SyncFunc is the function signature for syncing sources to a remote host.
// Tests can replace it with SetSyncFunc to avoid spawning rsync processes.
type SyncFunc func(host, localDir, remoteDir string, excludes []string) error

var syncFunc SyncFunc = defaultSyncFunc

// SetSyncFunc replaces the sync execution function.
// Returns a cleanup function that restores the original.
func SetSyncFunc(fn SyncFunc) func() {
	original := syncFunc
	syncFunc = fn
	return func() { syncFunc = original }
}

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
		// Job output directories (created by running jobs, not source files)
		"output", "outputs",
		// Editor and tool configs
		".vscode", ".claude", ".env",
		// AI/dev guidance files
		"CLAUDE.md", "AGENTS.md", "WARP.md",
		// Weft project config
		".weft.yaml",
	}
}

// rsyncSrcDst returns the source and destination arguments for rsync,
// ensuring trailing slashes so rsync syncs contents, not the directory itself.
func rsyncSrcDst(host, localDir, remoteDir string) (src, dst string) {
	src = strings.TrimRight(localDir, "/") + "/"
	dst = host + ":" + strings.TrimRight(remoteDir, "/") + "/"
	return
}

// BuildRsyncArgs constructs the rsync argument list for syncing sources to a remote host.
// host is the SSH hostname, localDir is an absolute local path (with trailing slash added),
// remoteDir may contain ~ (rsync expands it on the remote side).
func BuildRsyncArgs(host, localDir, remoteDir string, excludes []string) []string {
	args := []string{"-az", "--delete"}
	for _, pattern := range excludes {
		args = append(args, "--exclude", pattern)
	}
	src, dst := rsyncSrcDst(host, localDir, remoteDir)
	args = append(args, src, dst)
	return args
}

// SyncSources rsyncs localDir to host:remoteDir with standard excludes.
// localDir is an absolute local path. remoteDir may contain ~ (rsync expands it).
func SyncSources(host, localDir, remoteDir string) error {
	excludes := DefaultExcludes()
	// Also exclude project-specific output dirs from .weft.yaml
	for _, dir := range config.ProjectOutputDirs(localDir) {
		dir = strings.TrimSuffix(dir, "/")
		if !slices.Contains(excludes, dir) {
			excludes = append(excludes, dir)
		}
	}
	return syncFunc(host, localDir, remoteDir, excludes)
}

// BuildExtraPathRsyncArgs constructs rsync arguments for syncing an extra path
// to a remote host. Unlike BuildRsyncArgs, this does NOT use --delete since the
// remote directory may contain content from other sources.
func BuildExtraPathRsyncArgs(host, localDir, remoteDir string) []string {
	args := []string{"-az"}
	src, dst := rsyncSrcDst(host, localDir, remoteDir)
	args = append(args, src, dst)
	return args
}

// SyncExtraPaths rsyncs a list of local paths to the same paths on a remote host.
// Paths may use ~ (expanded locally via os.UserHomeDir). Each path is synced
// independently. Unlike SyncSources, --delete is NOT used since the remote
// directory may contain content from other sources.
func SyncExtraPaths(host string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home dir: %w", err)
	}

	for _, p := range paths {
		localPath := p
		remotePath := p

		// Expand ~ in local path
		if strings.HasPrefix(localPath, "~/") {
			localPath = filepath.Join(home, localPath[2:])
		}

		if err := syncFunc(host, localPath, remotePath, nil); err != nil {
			return fmt.Errorf("rsync extra path %s to %s: %w", p, host, err)
		}
	}
	return nil
}

func defaultSyncFunc(host, localDir, remoteDir string, excludes []string) error {
	var args []string
	if excludes != nil {
		args = BuildRsyncArgs(host, localDir, remoteDir, excludes)
	} else {
		args = BuildExtraPathRsyncArgs(host, localDir, remoteDir)
	}

	// Use a timeout so a slow or unresponsive host doesn't block the caller
	// indefinitely. Rsync's own --timeout covers data stalls but not initial
	// SSH connection hangs, so we use a context deadline for the whole process.
	ctx, cancel := context.WithTimeout(context.Background(), rsyncTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rsync", args...)
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rsync to %s:%s: %w", host, remoteDir, err)
	}
	return nil
}

// rsyncTimeout is the maximum time to wait for a single rsync operation.
// This prevents indefinite hangs when the remote host is unresponsive.
const rsyncTimeout = 30 * time.Second
