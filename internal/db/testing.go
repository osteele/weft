package db

import "sync"

// dbPathMu protects dbPath from concurrent access during tests.
// Tests that modify dbPath should hold this mutex.
var dbPathMu sync.Mutex

// SetDBPath allows tests in other packages to override the database path.
// Returns a cleanup function that restores the original path.
// This should only be used in tests.
// Note: This holds a mutex lock until cleanup is called, serializing
// tests that use different database paths.
func SetDBPath(path string) func() {
	dbPathMu.Lock()
	oldPath := dbPath
	dbPath = path
	return func() {
		dbPath = oldPath
		dbPathMu.Unlock()
	}
}
