package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	gosync "sync"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ssh"
)

// rsyncConnectTimeout governs ssh connection establishment for rsync. Longer
// than per-command pool timeouts because rsync sessions tolerate more latency
// (bulk transfer over potentially-slow links).
const rsyncConnectTimeout = 15 * time.Second

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
		// Go build caches
		".gocache", ".gomodcache",
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
		// Test data
		"testdata",
		// Editor and tool configs
		".vscode", ".claude", ".env",
		// AI/dev guidance files
		"CLAUDE.md", "AGENTS.md", "WARP.md",
		// Weft project config
		".weft.toml", ".weft.yaml",
		// Per-job source-provenance markers (written by the dispatcher on
		// the remote host; never present in the local tree). Excluding them
		// keeps rsync --delete from removing other queued jobs' markers
		// when this job's sync runs into a shared working dir.
		".weft-source.sha256", ".weft-source.*.sha256",
	}
}

// gitignoreFilters returns rsync --filter directives that make rsync respect
// git exclusion rules (.gitignore, .git/info/exclude, global gitignore).
// The returned slice contains interleaved flag/value pairs ready to append
// to an rsync args slice.
func gitignoreFilters(localDir string) []string {
	var filters []string

	// Per-directory .gitignore files (dir-merge rule: applied in each subdirectory)
	filters = append(filters, "--filter", ":- .gitignore")

	// Repo-level .git/info/exclude (only if .git/ exists)
	excludeFile := filepath.Join(localDir, ".git", "info", "exclude")
	if _, err := os.Stat(excludeFile); err == nil {
		filters = append(filters, "--filter", ".- "+excludeFile)
	}

	// Global gitignore
	if globalIgnore := globalGitIgnorePath(); globalIgnore != "" {
		filters = append(filters, "--filter", ".- "+globalIgnore)
	}

	return filters
}

var (
	globalGitIgnoreOnce  gosync.Once
	globalGitIgnoreValue string
)

// globalGitIgnorePath returns the absolute path to the user's global gitignore
// file, or "" if none exists or git is not installed. The result is cached
// since the global gitignore path is constant for the process lifetime.
func globalGitIgnorePath() string {
	globalGitIgnoreOnce.Do(func() {
		globalGitIgnoreValue = resolveGlobalGitIgnorePath()
	})
	return globalGitIgnoreValue
}

func resolveGlobalGitIgnorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	if out, err := exec.Command("git", "config", "--global", "core.excludesFile").Output(); err == nil {
		p := strings.TrimSpace(string(out))
		if p != "" {
			p = ExpandTildeDir(p)
			if p != "" {
				if _, err := os.Stat(p); err == nil {
					return p
				}
			}
		}
	}

	// Fallback: XDG default location
	defaultPath := filepath.Join(home, ".config", "git", "ignore")
	if _, err := os.Stat(defaultPath); err == nil {
		return defaultPath
	}
	return ""
}

// rsyncSrcDst returns the source and destination arguments for rsync,
// ensuring trailing slashes so rsync syncs contents, not the directory itself.
func rsyncSrcDst(host, localDir, remoteDir string) (src, dst string) {
	src = strings.TrimRight(localDir, "/") + "/"
	dst = ssh.RsyncTarget(host) + ":" + strings.TrimRight(remoteDir, "/") + "/"
	return
}

// BuildRsyncArgs constructs the rsync argument list for syncing sources to a remote host.
// host is the SSH hostname, localDir is an absolute local path (with trailing slash added),
// remoteDir may contain ~ (rsync expands it on the remote side).
func BuildRsyncArgs(host, localDir, remoteDir string, excludes []string) []string {
	return BuildRsyncArgsWithOptions(host, localDir, remoteDir, excludes, true)
}

// BuildRsyncArgsWithOptions constructs rsync arguments with an optional --delete.
func BuildRsyncArgsWithOptions(host, localDir, remoteDir string, excludes []string, delete bool) []string {
	args := []string{"-az", "-e", ssh.BatchModeRsyncCommandForHost(host, rsyncConnectTimeout)}
	if delete {
		args = append(args, "--delete")
	}
	if excludes != nil {
		args = append(args, gitignoreFilters(localDir)...)
	}
	for _, pattern := range excludes {
		args = append(args, "--exclude", pattern)
	}
	src, dst := rsyncSrcDst(host, localDir, remoteDir)
	args = append(args, src, dst)
	return args
}

// sourceExcludes returns DefaultExcludes plus .gitignore patterns and any
// project-specific output dirs.
func sourceExcludes(localDir string) []string {
	excludes := DefaultExcludes()
	if cfg, err := config.Load(); err == nil {
		for _, pattern := range cfg.SourceExcludeDirs() {
			if !slices.Contains(excludes, pattern) {
				excludes = append(excludes, pattern)
			}
		}
	}
	for _, dir := range config.ProjectExcludeDirs(localDir) {
		dir = strings.TrimSuffix(dir, "/")
		if dir != "" && !slices.Contains(excludes, dir) {
			excludes = append(excludes, dir)
		}
	}
	for _, dir := range config.ProjectOutputDirs(localDir) {
		dir = strings.TrimSuffix(dir, "/")
		if !slices.Contains(excludes, dir) {
			excludes = append(excludes, dir)
		}
	}
	for _, pattern := range parseGitignorePatterns(localDir) {
		if !slices.Contains(excludes, pattern) {
			excludes = append(excludes, pattern)
		}
	}
	return excludes
}

// parseGitignorePatterns reads the .gitignore file in localDir and returns
// patterns compatible with shouldExclude (basename glob patterns).
// Negation patterns, path-based patterns, and comments are skipped.
func parseGitignorePatterns(localDir string) []string {
	data, err := os.ReadFile(filepath.Join(localDir, ".gitignore"))
	if err != nil {
		return nil
	}
	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// Skip negation patterns (e.g. "!important.txt")
		if strings.HasPrefix(line, "!") {
			continue
		}
		// Strip trailing slash (directory indicator) — shouldExclude
		// matches against path components regardless
		line = strings.TrimSuffix(line, "/")
		if line == "" {
			continue
		}
		// Skip path-based patterns (contain a slash) — shouldExclude
		// only matches against individual path components / basenames
		if strings.Contains(line, "/") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns
}

// SyncSources rsyncs localDir to host:remoteDir with standard excludes.
// localDir is an absolute local path. remoteDir may contain ~ (rsync expands it).
func SyncSources(host, localDir, remoteDir string) error {
	excludes := sourceExcludes(localDir)
	start := time.Now()
	err := syncFunc(host, localDir, remoteDir, excludes)
	dur := time.Since(start)
	oplog.Log("sync.sources",
		oplog.WithHost(host),
		oplog.WithDuration(dur),
		oplog.WithDetailf("%s -> %s", localDir, remoteDir),
		oplog.WithError(err),
	)
	return err
}

// BuildExtraPathRsyncArgs constructs rsync arguments for syncing an extra path
// to a remote host. Unlike BuildRsyncArgs, this does NOT use --delete since the
// remote directory may contain content from other sources.
func BuildExtraPathRsyncArgs(host, localDir, remoteDir string) []string {
	args := []string{"-az", "-e", ssh.BatchModeRsyncCommandForHost(host, rsyncConnectTimeout)}
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

		start := time.Now()
		err := syncFunc(host, localPath, remotePath, nil)
		oplog.Log("sync.extra_path",
			oplog.WithHost(host),
			oplog.WithDuration(time.Since(start)),
			oplog.WithDetail(p),
			oplog.WithError(err),
		)
		if err != nil {
			return fmt.Errorf("rsync extra path %s to %s: %w", p, host, err)
		}
	}
	return nil
}

func defaultSyncFunc(host, localDir, remoteDir string, excludes []string) error {
	return defaultSyncFuncWithDelete(host, localDir, remoteDir, excludes, true)
}

func defaultSyncFuncWithDelete(host, localDir, remoteDir string, excludes []string, delete bool) error {
	var args []string
	if excludes != nil {
		args = BuildRsyncArgsWithOptions(host, localDir, remoteDir, excludes, delete)
	} else {
		args = BuildRsyncArgsWithOptions(host, localDir, remoteDir, nil, delete)
	}

	// Use a timeout so a slow or unresponsive host doesn't block the caller
	// indefinitely. Rsync's own --timeout covers data stalls but not initial
	// SSH connection hangs, so we use a context deadline for the whole process.
	ctx, cancel := context.WithTimeout(context.Background(), rsyncTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rsync", args...)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderrBuf.String())
		if isIgnorableRsyncError(err, msg) {
			return nil
		}
		if msg != "" {
			return fmt.Errorf("rsync to %s:%s: %s: %w", host, remoteDir, msg, err)
		}
		return fmt.Errorf("rsync to %s:%s: %w", host, remoteDir, err)
	}
	return nil
}

func isIgnorableRsyncError(err error, stderr string) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	if exitErr.ExitCode() != 24 {
		return false
	}

	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "file has vanished") ||
		strings.Contains(lower, "vanished before they could be transferred")
}

// SyncTree rsyncs a directory tree to a remote host. The local snapshot is
// assumed to already contain the desired contents, so no exclude rules apply.
func SyncTree(host, localDir, remoteDir string, delete bool) error {
	start := time.Now()
	err := defaultSyncFuncWithDelete(host, localDir, remoteDir, nil, delete)
	oplog.Log("sync.tree",
		oplog.WithHost(host),
		oplog.WithDuration(time.Since(start)),
		oplog.WithDetailf("%s -> %s delete=%t", localDir, remoteDir, delete),
		oplog.WithError(err),
	)
	return err
}

// SyncFile copies a single file to a remote path on the target host.
func SyncFile(host, localPath, remotePath string) error {
	args := []string{"-az", "-e", ssh.BatchModeRsyncCommandForHost(host, rsyncConnectTimeout), localPath, ssh.RsyncTarget(host) + ":" + remotePath}

	ctx, cancel := context.WithTimeout(context.Background(), rsyncTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rsync", args...)
	start := time.Now()
	err := cmd.Run()
	oplog.Log("sync.file",
		oplog.WithHost(host),
		oplog.WithDuration(time.Since(start)),
		oplog.WithDetailf("%s -> %s", localPath, remotePath),
		oplog.WithError(err),
	)
	if err != nil {
		return fmt.Errorf("rsync file to %s:%s: %w", host, remotePath, err)
	}
	return nil
}

// rsyncTimeout is the maximum time to wait for a single rsync operation.
// This prevents indefinite hangs when the remote host is unresponsive.
const rsyncTimeout = 30 * time.Second

// SyncSourcesWithSSH rsyncs localDir to a remote host with a custom SSH command.
// sshCmd is the full SSH command string (e.g., "ssh -p 12345 -o StrictHostKeyChecking=no").
// target is "user@host". remoteDir is the destination directory.
func SyncSourcesWithSSH(target, localDir, remoteDir, sshCmd string) error {
	excludes := sourceExcludes(localDir)

	args := []string{"-az", "--delete", "-e", sshCmd}
	args = append(args, gitignoreFilters(localDir)...)
	for _, pattern := range excludes {
		args = append(args, "--exclude", pattern)
	}
	src := strings.TrimRight(localDir, "/") + "/"
	dst := target + ":" + strings.TrimRight(remoteDir, "/") + "/"
	args = append(args, src, dst)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "rsync", args...)
	start := time.Now()
	out, err := cmd.CombinedOutput()
	dur := time.Since(start)
	oplog.Log("sync.sources_ssh",
		oplog.WithHost(target),
		oplog.WithDuration(dur),
		oplog.WithDetailf("%s -> %s", localDir, remoteDir),
		oplog.WithError(err),
	)
	if err != nil {
		return fmt.Errorf("rsync to %s:%s: %w\n%s", target, remoteDir, err, strings.TrimSpace(string(out)))
	}
	return nil
}
