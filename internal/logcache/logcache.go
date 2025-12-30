// Package logcache provides persistent local caching of completed job log files.
// This is distinct from TUI's in-memory cache which is for partial results
// of running jobs (offline fallback). This file cache stores complete logs
// of finished jobs for faster access without SSH.
package logcache

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/ssh"
)

// DefaultMaxAge is the default time to keep cached logs
const DefaultMaxAge = 7 * 24 * time.Hour // 7 days

// DefaultMaxSize is the default maximum log size to cache (50KB)
const DefaultMaxSize = 50 * 1024

// CacheDir returns the local cache directory for log files
func CacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "remote-jobs", "logs")
}

// CachePath returns the path to a cached log file for a job
func CachePath(jobID int64) string {
	return filepath.Join(CacheDir(), fmt.Sprintf("%d.log", jobID))
}

// Exists checks if a log is cached locally
func Exists(jobID int64) bool {
	_, err := os.Stat(CachePath(jobID))
	return err == nil
}

// Read returns cached log content, or error if not cached
func Read(jobID int64) (string, error) {
	data, err := os.ReadFile(CachePath(jobID))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Write caches log content for a job
func Write(jobID int64, content string) error {
	cacheDir := CacheDir()
	if cacheDir == "" {
		return fmt.Errorf("unable to determine cache directory")
	}

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}

	return os.WriteFile(CachePath(jobID), []byte(content), 0644)
}

// Delete removes a cached log file
func Delete(jobID int64) error {
	err := os.Remove(CachePath(jobID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// CacheFromRemote fetches a log file from a remote host and caches it locally
// if it's under the size limit. Returns (cached, error).
// If the host is unreachable or the file is too large, returns (false, nil).
func CacheFromRemote(host string, remotePath string, jobID int64, maxSize int) (bool, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}

	// Check file size first
	sizeCmd := fmt.Sprintf("stat -c %%s %s 2>/dev/null || stat -f %%z %s 2>/dev/null", remotePath, remotePath)
	stdout, _, err := ssh.Run(host, sizeCmd)
	if err != nil {
		// Host unreachable or file doesn't exist - not an error, just skip
		return false, nil
	}

	size, err := strconv.ParseInt(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		return false, nil
	}

	if size > int64(maxSize) {
		// File too large to cache
		return false, nil
	}

	// Fetch the content
	catCmd := fmt.Sprintf("cat %s", remotePath)
	content, _, err := ssh.Run(host, catCmd)
	if err != nil {
		return false, nil
	}

	// Cache it locally
	if err := Write(jobID, content); err != nil {
		return false, err
	}

	return true, nil
}

// Prune deletes cached logs older than maxAge.
// Returns the number of files deleted.
func Prune(maxAge time.Duration) (int, error) {
	cacheDir := CacheDir()
	if cacheDir == "" {
		return 0, nil
	}

	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}

	cutoff := time.Now().Add(-maxAge)
	pruned := 0

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			path := filepath.Join(cacheDir, entry.Name())
			if err := os.Remove(path); err == nil {
				pruned++
			}
		}
	}

	return pruned, nil
}
