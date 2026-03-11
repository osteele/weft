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
// current working directory, then falls back to empty string (remote home).
// logDest receives an "Auto-detected" message when automap fires; nil suppresses it.
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
				resolved := prefix + "/" + rel
				if logDest != nil {
					fmt.Fprintf(logDest, "Auto-detected working directory: %s\n", resolved)
				}
				return resolved, nil
			}
		}
	}
	return "", nil
}

// Normalize converts a submission directory into the canonical stored form.
// Relative local paths are resolved against the current working directory.
// Tilde-prefixed and absolute paths are preserved.
func Normalize(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	if strings.HasPrefix(dir, "~") || filepath.IsAbs(dir) {
		return filepath.Clean(dir), nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve absolute path for %q: %w", dir, err)
	}
	return abs, nil
}
