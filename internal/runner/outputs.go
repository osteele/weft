package runner

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
)

// DiscoverOutputs walks the configured output directories under workDir
// and returns a list of files with their sizes. Only regular files are included.
func DiscoverOutputs(workDir string, dirs []string) ([]OutputFile, error) {
	if workDir == "" || len(dirs) == 0 {
		return nil, nil
	}

	workDir = ExpandTilde(workDir)

	var files []OutputFile
	for _, dir := range dirs {
		dir = strings.TrimSuffix(dir, "/")
		absDir := filepath.Join(workDir, dir)

		info, err := os.Stat(absDir)
		if err != nil || !info.IsDir() {
			continue
		}

		err = filepath.WalkDir(absDir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(workDir, path)
			if err != nil {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			files = append(files, OutputFile{
				RelPath:   rel,
				SizeBytes: fi.Size(),
			})
			return nil
		})
		if err != nil {
			return files, err
		}
	}
	return files, nil
}

// ExpandTilde replaces a leading "~" with the user's home directory.
func ExpandTilde(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// DiscoverOutputsWithDefaults discovers outputs using the project config defaults.
func DiscoverOutputsWithDefaults(workDir string) ([]OutputFile, error) {
	dirs := config.ProjectOutputDirs(workDir)
	return DiscoverOutputs(workDir, dirs)
}

// anyOutputMtimeAfter reports whether any regular file under the configured
// output directories has an mtime strictly after threshold. Bounded to a
// 5k-entry per-call cap and short-circuits on the first match so the
// hang-watchdog probe stays cheap on large output trees.
func anyOutputMtimeAfter(workDir string, dirs []string, threshold time.Time) bool {
	if workDir == "" || len(dirs) == 0 {
		return false
	}
	workDir = ExpandTilde(workDir)
	const maxEntries = 5000
	scanned := 0
	found := false
	for _, dir := range dirs {
		absDir := filepath.Join(workDir, dir)
		info, err := os.Stat(absDir)
		if err != nil || !info.IsDir() {
			continue
		}
		filepath.WalkDir(absDir, func(_ string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if scanned >= maxEntries {
				return filepath.SkipAll
			}
			scanned++
			if d.IsDir() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			if fi.ModTime().After(threshold) {
				found = true
				return filepath.SkipAll
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}

// TotalSizeMB returns the total size of all output files in megabytes.
func TotalSizeMB(files []OutputFile) int {
	var total int64
	for _, f := range files {
		total += f.SizeBytes
	}
	return int(total / (1024 * 1024))
}
