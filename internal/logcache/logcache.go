// Package logcache provides persistent local caching of completed job log files.
// This is distinct from TUI's in-memory cache which is for partial results
// of running jobs (offline fallback). This file cache stores complete logs
// of finished jobs for faster access without SSH.
package logcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/ssh"
)

// DefaultMaxAge is the default time to keep cached logs
const DefaultMaxAge = 7 * 24 * time.Hour // 7 days

// DefaultMaxSize is the default maximum log size to cache (1MB)
const DefaultMaxSize = 1 * 1024 * 1024

// cacheMeta is the JSON structure stored in .log.meta sidecar files.
type cacheMeta struct {
	Complete bool   `json:"complete"`
	RunID    *int64 `json:"run_id,omitempty"`
}

// CacheDir returns the local cache directory for log files
func CacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".cache", "weft", "logs")
}

// CachePath returns the path to a cached log file for a job
func CachePath(jobID int64) string {
	return filepath.Join(CacheDir(), fmt.Sprintf("%d.log", jobID))
}

// metaPath returns the path to the metadata sidecar file for a job
func metaPath(jobID int64) string {
	return CachePath(jobID) + ".meta"
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

// Write caches log content for a job, marking it as a complete log.
func Write(jobID int64, content string) error {
	return WriteWithMeta(jobID, content, true)
}

// WriteWithMeta caches log content for a job with explicit completeness metadata.
func WriteWithMeta(jobID int64, content string, isComplete bool) error {
	return writeWithMeta(jobID, content, isComplete, nil)
}

// WriteForRun caches log content for a specific run/attempt ID, marking it complete.
func WriteForRun(jobID, runID int64, content string) error {
	return writeWithMeta(jobID, content, true, &runID)
}

// WriteWithMetaForRun caches log content for a specific run/attempt ID.
func WriteWithMetaForRun(jobID int64, runID *int64, content string, isComplete bool) error {
	return writeWithMeta(jobID, content, isComplete, runID)
}

func writeWithMeta(jobID int64, content string, isComplete bool, runID *int64) error {
	cacheDir := CacheDir()
	if cacheDir == "" {
		return fmt.Errorf("unable to determine cache directory")
	}

	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return fmt.Errorf("create cache directory: %w", err)
	}

	if err := os.WriteFile(CachePath(jobID), []byte(content), 0644); err != nil {
		return err
	}

	meta := cacheMeta{Complete: isComplete, RunID: runID}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath(jobID), data, 0644)
}

// IsComplete returns whether the cached log is known to be the full file.
// Returns false if there is no metadata file or the cache doesn't exist.
func IsComplete(jobID int64) bool {
	data, err := os.ReadFile(metaPath(jobID))
	if err != nil {
		return false
	}
	var meta cacheMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return false
	}
	return meta.Complete
}

// RunID returns the cached run/attempt ID if metadata includes one.
func RunID(jobID int64) (int64, bool) {
	data, err := os.ReadFile(metaPath(jobID))
	if err != nil {
		return 0, false
	}
	var meta cacheMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return 0, false
	}
	if meta.RunID == nil {
		return 0, false
	}
	return *meta.RunID, true
}

// Delete removes a cached log file and its metadata sidecar
func Delete(jobID int64) error {
	metaErr := os.Remove(metaPath(jobID))
	if metaErr != nil && !os.IsNotExist(metaErr) {
		return metaErr
	}
	err := os.Remove(CachePath(jobID))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// CacheFromRemote fetches a log file from a remote host and caches it locally
// if it's under the size limit. Returns (cached, error).
// If the host is unreachable or the file is too large, returns (false, nil).
// A zero timeout uses the default SSH timeout.
func CacheFromRemote(host string, remotePath string, jobID int64, maxSize int, timeout time.Duration) (bool, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}

	run := func(host, cmd string) (string, string, error) {
		if timeout > 0 {
			return ssh.RunWithTimeout(host, cmd, timeout)
		}
		return ssh.Run(host, cmd)
	}

	// Check file size first
	sizeCmd := fmt.Sprintf("stat -c %%s %s 2>/dev/null || stat -f %%z %s 2>/dev/null", remotePath, remotePath)
	stdout, _, err := run(host, sizeCmd)
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
	content, _, err := run(host, catCmd)
	if err != nil {
		return false, nil
	}

	// Cache it locally (full file, so mark complete)
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
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		// Clean up both .log and .log.meta files based on .log file age
		if !strings.HasSuffix(name, ".log") {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		if info.ModTime().Before(cutoff) {
			logPath := filepath.Join(cacheDir, name)
			if err := os.Remove(logPath); err == nil {
				pruned++
			}
			// Also remove the meta sidecar
			_ = os.Remove(logPath + ".meta")
		}
	}

	return pruned, nil
}
