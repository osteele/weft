package dbwatch

import (
	"path/filepath"

	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/db"
)

// OpenJobsDBWatcher watches the jobs DB directory and returns the watcher and
// the set of DB-related files that should trigger refresh.
func OpenJobsDBWatcher() (*fsnotify.Watcher, map[string]struct{}, error) {
	dbFile := db.Path()
	if dbFile == "" {
		return nil, nil, nil
	}

	dir := filepath.Dir(dbFile)
	targets := Targets(dbFile)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	if err := watcher.Add(dir); err != nil {
		_ = watcher.Close()
		return nil, nil, err
	}
	return watcher, targets, nil
}

// Targets returns the DB file names that should be treated as refresh events.
func Targets(dbFile string) map[string]struct{} {
	if dbFile == "" {
		return nil
	}
	dir := filepath.Dir(dbFile)
	targets := make(map[string]struct{}, 3)
	addTarget := func(name string) {
		if name == "" {
			return
		}
		targets[filepath.Clean(filepath.Join(dir, name))] = struct{}{}
	}
	base := filepath.Base(dbFile)
	addTarget(base)
	addTarget(base + "-wal")
	addTarget(base + "-shm")
	return targets
}

// IsWatchedFile reports whether the path belongs to the DB files being watched.
func IsWatchedFile(name string, targets map[string]struct{}) bool {
	if name == "" || len(targets) == 0 {
		return false
	}
	_, ok := targets[filepath.Clean(name)]
	return ok
}
