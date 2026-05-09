package dbwatch

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
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
