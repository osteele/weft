package agentdeploy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/util"
)

// ErrAgentNotAvailable is returned when the agent binary for a platform is not
// available in the binaries/ directory. Callers should warn and continue rather
// than treating this as a fatal error.
var ErrAgentNotAvailable = errors.New("agent binary not available for this platform")

// ExtractFunc is the function signature for extracting an agent binary.
// Tests can replace it with SetExtractFunc to avoid requiring real binaries.
type ExtractFunc func(version, goos, goarch, outputPath string) error

var (
	extractFunc       ExtractFunc = defaultExtractFunc
	buildRepoRootFunc             = RepoRoot
)

// SetExtractFunc replaces the extract execution function.
// Returns a cleanup function that restores the original.
func SetExtractFunc(fn ExtractFunc) func() {
	original := extractFunc
	extractFunc = fn
	return func() { extractFunc = original }
}

// CachePath returns the local cache path for a built agent binary.
// Layout: ~/.cache/weft/builds/<version>/<goos>-<goarch>/weft-agent
func CachePath(version, goos, goarch string) string {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = filepath.Join(os.Getenv("HOME"), ".cache")
	}
	key := goos + "-" + goarch
	return filepath.Join(cacheDir, "weft", "builds", version, key, "weft-agent")
}

// EnsureBuilt checks the local build cache and extracts the agent binary from
// the binaries/ directory if the cached binary is missing. Returns the path to
// the binary, or ErrAgentNotAvailable if no binary exists for this platform.
// On-demand build progress is discarded; use EnsureBuiltWithOutput to stream it.
func EnsureBuilt(version, goos, goarch string) (string, error) {
	return EnsureBuiltWithProgress(version, goos, goarch, io.Discard, nil)
}

// EnsureBuiltWithOutput is like EnsureBuilt but streams on-demand build progress
// (Fly builder start, source rsync, compile, download, stop) to output.
func EnsureBuiltWithOutput(version, goos, goarch string, output io.Writer) (string, error) {
	return EnsureBuiltWithProgress(version, goos, goarch, output, nil)
}

// EnsureBuiltWithProgress is like EnsureBuiltWithOutput but also reports
// coarse phase changes via onProgress. Callers that render their own UI
// (e.g. TUIs) typically pass io.Discard for output and use onProgress.
func EnsureBuiltWithProgress(version, goos, goarch string, output io.Writer, onProgress BuildProgressFunc) (string, error) {
	if output == nil {
		output = io.Discard
	}
	if onProgress == nil {
		onProgress = func(string) {}
	}
	path := CachePath(version, goos, goarch)

	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	releaseLock, err := acquireBuildLock(path)
	if err != nil {
		return "", fmt.Errorf("acquire build lock: %w", err)
	}
	defer releaseLock()

	// Another process may have populated the cache while we waited for the lock.
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	if err := extractFunc(version, goos, goarch, path); err != nil {
		if errBuild := buildOnDemand(version, goos, goarch, path, output, onProgress); errBuild != nil {
			if errors.Is(err, ErrAgentNotAvailable) {
				return "", errBuild
			}
			return "", fmt.Errorf("extract bundled agent binary: %w; on-demand build failed: %v", err, errBuild)
		}
	}

	return path, nil
}

// acquireBuildLock serializes concurrent builds targeting the same output path.
// The lock is a single file (atomically created via O_CREATE|O_EXCL) whose
// contents are the holder's PID. Waiters reclaim the lock when the holder
// process is gone, preventing crashed builds from blocking subsequent attempts
// for the full wait timeout.
func acquireBuildLock(outputPath string) (func(), error) {
	lockFile := outputPath + ".lock"
	const (
		waitInterval = 200 * time.Millisecond
		waitTimeout  = 10 * time.Minute
	)
	deadline := time.Now().Add(waitTimeout)

	for {
		f, err := os.OpenFile(lockFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = f.WriteString(strconv.Itoa(os.Getpid()))
			_ = f.Close()
			return func() { _ = os.Remove(lockFile) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}

		// If another process completed while we were waiting, no need to keep waiting.
		if _, err := os.Stat(outputPath); err == nil {
			return func() {}, nil
		}

		if shouldReclaimBuildLock(lockFile) {
			_ = os.Remove(lockFile)
			continue
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting for build lock %s", lockFile)
		}
		time.Sleep(waitInterval)
	}
}

// shouldReclaimBuildLock reports whether a held lock can be safely taken over.
// A live recorded PID always owns the lock, even if the lock file is old. Slow
// remote agent builds can legitimately take several minutes; reclaiming by age
// starts duplicate builders that fight over the same cache path and remote VM.
func shouldReclaimBuildLock(lockFile string) bool {
	info, err := os.Stat(lockFile)
	if err != nil {
		return false // disappeared on its own; loop will try OpenFile again
	}
	data, err := os.ReadFile(lockFile)
	if err != nil {
		return true
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		// Empty file (writer raced us between OpenFile and WriteString) —
		// give the writer a small grace window before reclaiming.
		return time.Since(info.ModTime()) > 500*time.Millisecond
	}
	return !util.IsProcessAlive(pid)
}

// BinariesVersion returns the version recorded in the binaries/ directory.
// Returns os.ErrNotExist if no VERSION file is present.
func BinariesVersion() (string, error) {
	root, err := buildRepoRootFunc()
	if err != nil {
		return "", err
	}
	versionFile := filepath.Join(root, "internal", "agentdeploy", "binaries", "VERSION")
	data, err := os.ReadFile(versionFile)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// CheckAgentBinariesCurrent verifies that the local agent binary for
// linux/amd64 is available and matches the current version.
func CheckAgentBinariesCurrent() error {
	version, err := LocalAgentVersion()
	if err != nil {
		return fmt.Errorf("determine agent version: %w", err)
	}

	// Already in the build cache — good to go.
	if _, err := os.Stat(CachePath(version, "linux", "amd64")); err == nil {
		return nil
	}

	// Check embedded binaries directory.
	builtVersion, err := BinariesVersion()
	if err == nil && builtVersion == version {
		return nil
	}
	return fmt.Errorf("%w for linux/amd64", ErrAgentNotAvailable)
}

func defaultExtractFunc(version, goos, goarch, outputPath string) error {
	root, err := buildRepoRootFunc()
	if err != nil {
		return fmt.Errorf("%w: source tree unavailable: %w", ErrAgentNotAvailable, err)
	}

	binDir := filepath.Join(root, "internal", "agentdeploy", "binaries")
	binName := fmt.Sprintf("weft-agent-%s-%s", goos, goarch)
	srcPath := filepath.Join(binDir, binName)

	// Check VERSION matches before opening the binary
	versionFile := filepath.Join(binDir, "VERSION")
	if versionData, err := os.ReadFile(versionFile); err == nil {
		if builtVersion := strings.TrimSpace(string(versionData)); builtVersion != version {
			return fmt.Errorf("%w for %s/%s: binary in binaries/ is version %s, need %s",
				ErrAgentNotAvailable, goos, goarch, builtVersion, version)
		}
	}

	src, err := os.Open(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w for %s/%s: binary not found in internal/agentdeploy/binaries", ErrAgentNotAvailable, goos, goarch)
		}
		return fmt.Errorf("%w for %s/%s: %w", ErrAgentNotAvailable, goos, goarch, err)
	}
	defer src.Close()

	// Write to a temp file and rename atomically to avoid partial writes
	// from concurrent extractions or interrupted processes.
	tmpPath := outputPath + ".tmp"
	dst, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create agent binary: %w", err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write agent binary: %w", err)
	}
	if err := dst.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close agent binary: %w", err)
	}

	if err := os.Rename(tmpPath, outputPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("install agent binary: %w", err)
	}
	return nil
}
