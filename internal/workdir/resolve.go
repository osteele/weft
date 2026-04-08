package workdir

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/config"
)

// ResolveWorkingDir resolves the effective working directory for a job submission.
// If dir is non-empty it is returned as-is. Otherwise it tries automap from the
// current working directory (based on the automap_dirs config setting), then
// falls back to empty string (remote home). logDest receives an "Auto-detected"
// message when automap fires; nil suppresses it.
func ResolveWorkingDir(dir string, logDest io.Writer) (string, error) {
	if dir != "" {
		return Normalize(dir)
	}
	home, _ := os.UserHomeDir()
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	if home != "" && cwd != "" {
		for _, prefix := range config.AutomapDirs() {
			expanded := expandTilde(prefix, home)
			if rel, err := filepath.Rel(expanded, cwd); err == nil && !strings.HasPrefix(rel, "..") {
				resolved := prefix
				if rel != "." {
					resolved = prefix + "/" + rel
				}
				if logDest != nil {
					fmt.Fprintf(logDest, "Auto-detected working directory: %s\n", resolved)
				}
				return resolved, nil
			}
		}
	}
	return "", nil
}

// containerPrefixes are path prefixes that indicate a container environment,
// not a local filesystem path. Jobs submitted with these paths will fail
// source tarball creation.
var containerPrefixes = []string{
	"/workspace/",
	"/app/",
	"/opt/ml/",
	"/home/user/",
}

// Normalize converts a submission directory into the canonical stored form.
// Relative local paths are resolved against the current working directory.
// Tilde-prefixed and absolute paths are preserved.
//
// Paths matching known container prefixes (e.g. /workspace/...) are rejected
// to catch jobs submitted from inside a cloud instance.
func Normalize(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if strings.HasPrefix(dir, "~") || filepath.IsAbs(dir) {
		cleaned := filepath.Clean(dir)
		if err := rejectContainerPath(cleaned); err != nil {
			return "", err
		}
		return cleaned, nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path for %q: %w", dir, err)
	}
	return abs, nil
}

// rejectContainerPath returns an error if the path looks like a container
// mount point rather than a local filesystem path.
func rejectContainerPath(path string) error {
	if IsContainerPath(path) {
		return fmt.Errorf("working directory %q looks like a container path, not a local path; use a ~ or local absolute path instead", path)
	}
	return nil
}

// IsContainerPath returns true if the path matches known container mount prefixes.
func IsContainerPath(path string) bool {
	for _, prefix := range containerPrefixes {
		if strings.HasPrefix(path, prefix) || path == strings.TrimSuffix(prefix, "/") {
			return true
		}
	}
	return false
}
