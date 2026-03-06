package workdir

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/config"
)

// ProjectName extracts a short project name from a working directory path.
// E.g. "~/code/research/llm-performance-models" → "llm-performance-models".
// If dir is empty, falls back to the base name of the current working directory.
// Returns "" if the directory cannot be determined.
func ProjectName(dir string) string {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return ""
		}
		return filepath.Base(cwd)
	}
	return filepath.Base(dir)
}

// ResolveLocal converts a tilde-prefixed working directory back to a local
// absolute path using the automap directory prefixes. Returns "" if the path
// cannot be resolved to a local directory.
func ResolveLocal(workingDir string) string {
	if workingDir == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	for _, prefix := range config.AutomapDirs() {
		expanded := expandTilde(prefix, home)
		if strings.HasPrefix(workingDir, prefix+"/") {
			rel := workingDir[len(prefix)+1:]
			return filepath.Join(expanded, rel)
		}
		if workingDir == prefix {
			return expanded
		}
	}
	if filepath.IsAbs(workingDir) {
		return workingDir
	}
	return ""
}

// expandTilde replaces a leading "~" in prefix with the given home directory.
func expandTilde(prefix, home string) string {
	return strings.Replace(prefix, "~", home, 1)
}
