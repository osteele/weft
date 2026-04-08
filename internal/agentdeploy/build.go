package agentdeploy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ErrAgentNotAvailable is returned when the agent binary for a platform is not
// available in the binaries/ directory. Callers should warn and continue rather
// than treating this as a fatal error.
var ErrAgentNotAvailable = errors.New("agent binary not available for this platform")

// ExtractFunc is the function signature for extracting an agent binary.
// Tests can replace it with SetExtractFunc to avoid requiring real binaries.
type ExtractFunc func(version, goos, goarch, outputPath string) error

var extractFunc ExtractFunc = defaultExtractFunc

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
func EnsureBuilt(version, goos, goarch string) (string, error) {
	path := CachePath(version, goos, goarch)

	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	if err := extractFunc(version, goos, goarch, path); err != nil {
		return "", err
	}

	return path, nil
}

// BinariesVersion returns the version recorded in the binaries/ directory.
// Returns os.ErrNotExist if no VERSION file is present.
func BinariesVersion() (string, error) {
	root, err := RepoRoot()
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
// linux/amd64 is available and matches the current version. Returns nil if
// ready, or a user-facing error explaining what to run.
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
	if err != nil {
		return fmt.Errorf("agent binaries not built; run 'just build-agents'")
	}
	if builtVersion != version {
		return fmt.Errorf("agent binaries are stale (built for %s, need %s); run 'just build-agents'", builtVersion, version)
	}

	return nil
}

func defaultExtractFunc(version, goos, goarch, outputPath string) error {
	root, err := RepoRoot()
	if err != nil {
		return fmt.Errorf("%w: run 'just build-agents' first: %w", ErrAgentNotAvailable, err)
	}

	binDir := filepath.Join(root, "internal", "agentdeploy", "binaries")
	binName := fmt.Sprintf("weft-agent-%s-%s", goos, goarch)
	srcPath := filepath.Join(binDir, binName)

	// Check VERSION matches before opening the binary
	versionFile := filepath.Join(binDir, "VERSION")
	if versionData, err := os.ReadFile(versionFile); err == nil {
		if builtVersion := strings.TrimSpace(string(versionData)); builtVersion != version {
			return fmt.Errorf("%w for %s/%s: binary in binaries/ is version %s, need %s; run 'just build-agents'",
				ErrAgentNotAvailable, goos, goarch, builtVersion, version)
		}
	}

	src, err := os.Open(srcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w for %s/%s: run 'just build-agents' first", ErrAgentNotAvailable, goos, goarch)
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
