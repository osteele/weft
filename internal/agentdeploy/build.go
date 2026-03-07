package agentdeploy

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ExtractFunc is the function signature for extracting an embedded agent binary.
// Tests can replace it with SetExtractFunc to avoid requiring real embedded binaries.
type ExtractFunc func(goos, goarch, outputPath string) error

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
	return filepath.Join(cacheDir, "weft", "builds", version, goos+"-"+goarch, "weft-agent")
}

// EnsureBuilt checks the local build cache and extracts the embedded agent
// binary if the cached binary is missing. Returns the path to the binary.
func EnsureBuilt(version, goos, goarch string) (string, error) {
	path := CachePath(version, goos, goarch)

	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create cache dir: %w", err)
	}

	if err := extractFunc(goos, goarch, path); err != nil {
		return "", err
	}

	return path, nil
}

func defaultExtractFunc(goos, goarch, outputPath string) error {
	name := fmt.Sprintf("binaries/weft-agent-%s-%s", goos, goarch)
	src, err := agentBinaries.Open(name)
	if err != nil {
		return fmt.Errorf("agent binary for %s/%s not embedded; run \"just build-agents\" then rebuild weft: %w", goos, goarch, err)
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
