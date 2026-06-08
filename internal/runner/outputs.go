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
		discovered, err := discoverOutputDir(workDir, dir)
		if err != nil {
			return files, err
		}
		files = append(files, discovered...)
	}
	return files, nil
}

// DiscoverOutputRefs walks declared output references under workDir. References
// may be either regular files or directories.
func DiscoverOutputRefs(workDir string, refs []string) ([]OutputFile, error) {
	if workDir == "" || len(refs) == 0 {
		return nil, nil
	}

	workDir = ExpandTilde(workDir)

	var files []OutputFile
	seen := make(map[string]bool)
	for _, ref := range refs {
		cleanRef, ok := FilesystemOutputRefPath(ref)
		if !ok {
			continue
		}

		cleanRef = strings.TrimSuffix(cleanRef, "/")
		absPath := filepath.Join(workDir, cleanRef)
		info, err := os.Stat(absPath)
		if err != nil {
			continue
		}

		var discovered []OutputFile
		if info.IsDir() {
			discovered, err = discoverOutputDir(workDir, cleanRef)
		} else if info.Mode().IsRegular() {
			rel, relErr := filepath.Rel(workDir, absPath)
			if relErr != nil {
				continue
			}
			discovered = []OutputFile{{
				RelPath:   filepath.ToSlash(rel),
				SizeBytes: info.Size(),
			}}
		}
		if err != nil {
			return files, err
		}
		for _, f := range discovered {
			if seen[f.RelPath] {
				continue
			}
			seen[f.RelPath] = true
			files = append(files, f)
		}
	}
	return files, nil
}

// FilesystemOutputRefPath returns the workspace-relative path named by a
// filesystem output reference. Plain paths and local: paths are filesystem
// outputs; other colon-qualified refs identify data assets.
func FilesystemOutputRefPath(ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	if strings.HasPrefix(ref, "local:") {
		ref = strings.TrimSpace(strings.TrimPrefix(ref, "local:"))
	}
	if ref == "" || strings.Contains(ref, ":") {
		return "", false
	}
	return ref, true
}

// DiscoverJobOutputs discovers convention directories and declared filesystem
// output references for a job.
func DiscoverJobOutputs(workDir string, dirs, refs []string) ([]OutputFile, error) {
	files, err := DiscoverOutputs(workDir, dirs)
	if err != nil {
		return nil, err
	}
	refsFiles, err := DiscoverOutputRefs(workDir, refs)
	if err != nil {
		return files, err
	}
	seen := make(map[string]bool, len(files)+len(refsFiles))
	deduped := make([]OutputFile, 0, len(files)+len(refsFiles))
	for _, f := range append(files, refsFiles...) {
		if seen[f.RelPath] {
			continue
		}
		seen[f.RelPath] = true
		deduped = append(deduped, f)
	}
	return deduped, nil
}

// DiscoverJobOutputsSince discovers job outputs and keeps only files modified
// at or after threshold. Use for attempt-scoped completion records so stale
// files in shared output directories are not attributed to a later job.
func DiscoverJobOutputsSince(workDir string, dirs, refs []string, threshold time.Time) ([]OutputFile, error) {
	files, err := DiscoverJobOutputs(workDir, dirs, refs)
	if err != nil {
		return nil, err
	}
	return FilterOutputFilesSince(workDir, files, threshold), nil
}

// FilterOutputFilesSince keeps output files whose mtime is at or after
// threshold. A zero threshold leaves the list unchanged.
func FilterOutputFilesSince(workDir string, files []OutputFile, threshold time.Time) []OutputFile {
	if threshold.IsZero() || workDir == "" || len(files) == 0 {
		return files
	}
	workDir = ExpandTilde(workDir)
	filtered := make([]OutputFile, 0, len(files))
	for _, file := range files {
		path := filepath.Join(workDir, filepath.FromSlash(file.RelPath))
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if info.ModTime().Before(threshold) {
			continue
		}
		filtered = append(filtered, file)
	}
	return filtered
}

func discoverOutputDir(workDir, dir string) ([]OutputFile, error) {
	absDir := filepath.Join(workDir, dir)

	info, err := os.Stat(absDir)
	if err != nil || !info.IsDir() {
		return nil, nil
	}

	var files []OutputFile
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
			RelPath:   filepath.ToSlash(rel),
			SizeBytes: fi.Size(),
		})
		return nil
	})
	return files, err
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
