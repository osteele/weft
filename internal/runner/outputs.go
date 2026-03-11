package runner

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/osteele/weft/internal/config"
)

// DiscoverOutputs walks the configured output directories under workDir
// and returns a list of files with their sizes. Only regular files are included.
func DiscoverOutputs(workDir string, dirs []string) ([]OutputFile, error) {
	if workDir == "" || len(dirs) == 0 {
		return nil, nil
	}

	workDir = expandTilde(workDir)

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

// expandTilde replaces a leading "~/" with the user's home directory.
func expandTilde(path string) string {
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

// TotalSizeMB returns the total size of all output files in megabytes.
func TotalSizeMB(files []OutputFile) int {
	var total int64
	for _, f := range files {
		total += f.SizeBytes
	}
	return int(total / (1024 * 1024))
}
