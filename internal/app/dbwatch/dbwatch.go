package dbwatch

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

const defaultDebounce = 200 * time.Millisecond

// Source is a coarse database change source. It reports that one of SQLite's
// on-disk files changed; consumers still reload from the database to derive
// the typed state they need.
type Source struct {
	watcher  *fsnotify.Watcher
	targets  map[string]struct{}
	debounce time.Duration
}

// OpenChangeSource opens a coarse change source for the jobs DB. A nil Source
// means there is no configured DB path to watch.
func OpenChangeSource() (*Source, error) {
	watcher, targets, err := OpenJobsDBWatcher()
	if err != nil {
		return nil, err
	}
	if watcher == nil {
		return nil, nil
	}
	return &Source{watcher: watcher, targets: targets, debounce: defaultDebounce}, nil
}

// Close releases the underlying fsnotify watcher.
func (s *Source) Close() error {
	if s == nil || s.watcher == nil {
		return nil
	}
	return s.watcher.Close()
}

// Wait blocks until the watched DB files change, maxWait elapses, or ctx is
// canceled.
func (s *Source) Wait(ctx context.Context, maxWait time.Duration) (bool, error) {
	if s == nil || s.watcher == nil || len(s.targets) == 0 {
		return waitWithoutSource(ctx, maxWait)
	}

	var timer <-chan time.Time
	var t *time.Timer
	if maxWait > 0 {
		t = time.NewTimer(maxWait)
		timer = t.C
		defer t.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-timer:
			return false, nil
		case event, ok := <-s.watcher.Events:
			if !ok {
				return false, fmt.Errorf("db watcher closed")
			}
			if !s.isRelevant(event) {
				continue
			}
			s.debounceEvents(ctx)
			return true, nil
		case err, ok := <-s.watcher.Errors:
			if !ok {
				return false, fmt.Errorf("db watcher error channel closed")
			}
			return false, err
		}
	}
}

func waitWithoutSource(ctx context.Context, maxWait time.Duration) (bool, error) {
	if maxWait <= 0 {
		<-ctx.Done()
		return false, ctx.Err()
	}
	t := time.NewTimer(maxWait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-t.C:
		return false, nil
	}
}

func (s *Source) drainPending() {
	for {
		select {
		case _, ok := <-s.watcher.Events:
			if !ok {
				return
			}
		case _, ok := <-s.watcher.Errors:
			if !ok {
				return
			}
		default:
			return
		}
	}
}

func (s *Source) debounceEvents(ctx context.Context) {
	if s.debounce <= 0 {
		s.drainPending()
		return
	}
	t := time.NewTimer(s.debounce)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
	s.drainPending()
}

func (s *Source) isRelevant(event fsnotify.Event) bool {
	if !IsWatchedFile(event.Name, s.targets) {
		return false
	}
	return event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0
}

// OpenJobsDBWatcher watches the jobs DB directory and returns the watcher and
// the set of files that should trigger refresh. The config file is watched
// alongside the database: settings such as the autopilot's run-rate target are
// decision inputs that never produce a database write, so an editor that
// watches only the database sleeps through them.
func OpenJobsDBWatcher() (*fsnotify.Watcher, map[string]struct{}, error) {
	dbFile := db.Path()
	if dbFile == "" {
		return nil, nil, nil
	}

	targets := Targets(dbFile)

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, err
	}
	// fsnotify watches directories, so collect the distinct parents of every
	// target. The config file usually lives beside the database, in which
	// case this is a single watch.
	dirs := make(map[string]struct{}, 2)
	for target := range targets {
		dirs[filepath.Dir(target)] = struct{}{}
	}
	dbDir := filepath.Dir(dbFile)
	dirs[dbDir] = struct{}{}
	for dir := range dirs {
		if err := watcher.Add(dir); err != nil {
			// A missing config directory must not cost us the database
			// watch, which is the one that carries most changes.
			if dir == dbDir {
				_ = watcher.Close()
				return nil, nil, err
			}
			continue
		}
	}
	return watcher, targets, nil
}

// Targets returns the file names that should be treated as refresh events.
func Targets(dbFile string) map[string]struct{} {
	if dbFile == "" {
		return nil
	}
	dir := filepath.Dir(dbFile)
	targets := make(map[string]struct{}, 4)
	addTarget := func(path string) {
		if path == "" {
			return
		}
		targets[filepath.Clean(path)] = struct{}{}
	}
	base := filepath.Base(dbFile)
	addTarget(filepath.Join(dir, base))
	addTarget(filepath.Join(dir, base+"-wal"))
	addTarget(filepath.Join(dir, base+"-shm"))
	addTarget(config.ConfigPath())
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
