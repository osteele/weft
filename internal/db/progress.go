package db

import "sync"

// MigrationProgressEvent describes a user-visible phase of database migration.
type MigrationProgressEvent struct {
	Phase       string
	FromVersion int
	ToVersion   int
	Path        string
	Error       string
}

// MigrationProgressReporter receives migration progress events. It must return
// quickly; database startup calls it synchronously before long migration work.
type MigrationProgressReporter func(MigrationProgressEvent)

var (
	migrationProgressMu       sync.Mutex
	migrationProgressReporter MigrationProgressReporter
)

// SetMigrationProgressReporter installs a process-wide migration progress
// reporter and returns a restore function for tests.
func SetMigrationProgressReporter(reporter MigrationProgressReporter) func() {
	migrationProgressMu.Lock()
	previous := migrationProgressReporter
	migrationProgressReporter = reporter
	migrationProgressMu.Unlock()
	return func() {
		migrationProgressMu.Lock()
		migrationProgressReporter = previous
		migrationProgressMu.Unlock()
	}
}

func reportMigrationProgress(event MigrationProgressEvent) {
	migrationProgressMu.Lock()
	reporter := migrationProgressReporter
	migrationProgressMu.Unlock()
	if reporter != nil {
		reporter(event)
	}
}
