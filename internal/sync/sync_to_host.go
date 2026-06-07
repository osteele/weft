package sync

import (
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
)

// SyncSourcesToHost syncs source files and extra paths to a remote host.
// localDir is the absolute local path; remoteDir is the path on the remote host.
// Skips syncing when the target host is the local machine (same hostname)
// to avoid rsyncing a directory to itself, and returns nil.
func SyncSourcesToHost(host, localDir, remoteDir string, inputs []string) error {
	if localDir == "" {
		return nil
	}
	if IsLocalHost(host) {
		return nil
	}
	if err := SyncSources(host, localDir, remoteDir); err != nil {
		return err
	}
	overlays, err := LocalInputOverlays(localDir, inputs, SkipMissingLocalInput)
	if err != nil {
		return err
	}
	if len(overlays) > 0 {
		if err := SyncLocalInputOverlays(host, remoteDir, overlays); err != nil {
			return err
		}
	}
	extraPaths := CollectExtraPaths(nonLocalInputs(inputs), localDir)
	if len(extraPaths) > 0 {
		if err := SyncExtraPaths(host, extraPaths); err != nil {
			return err
		}
	}
	return nil
}

var (
	cachedHostname     string
	cachedHostnameOnce gosync.Once
)

// IsLocalHost returns true if the given host is the local machine.
func IsLocalHost(host string) bool {
	if host == "" || host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	cachedHostnameOnce.Do(func() {
		cachedHostname, _ = os.Hostname()
	})
	return cachedHostname != "" && host == cachedHostname
}

// ExpandTildeDir expands a tilde-prefixed directory to an absolute path
// using the current user's home directory. Returns "" if the path cannot
// be resolved.
func ExpandTildeDir(dir string) string {
	if dir == "" {
		return ""
	}
	if strings.HasPrefix(dir, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, dir[2:])
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return ""
}
