package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// ErrJobNotFound is returned when a job ID does not exist in the database.
var ErrJobNotFound = errors.New("job not found")

// ErrJobAlreadyClaimed is returned by SetJobLaunchID when the job is already
// assigned to an active launch (launching/running/grace/completed).
var ErrJobAlreadyClaimed = errors.New("job already claimed by another launch")

// IsDatabaseLocked reports whether err is a SQLite SQLITE_BUSY error.
func IsDatabaseLocked(err error) bool {
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.Code() == sqlite3.SQLITE_BUSY
	}
	msg := strings.ToLower(errString(err))
	return strings.Contains(msg, "database is locked") || strings.Contains(msg, "sqlite_busy")
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// RetryOnDatabaseLocked retries fn when SQLite returns SQLITE_BUSY.
// Retries are unbounded and stop only when fn succeeds, returns a non-lock
// error, or ctx is canceled.
func RetryOnDatabaseLocked(ctx context.Context, op string, fn func() error) error {
	if fn == nil {
		return fmt.Errorf("%s: nil operation", strings.TrimSpace(op))
	}
	if ctx == nil {
		ctx = context.Background()
	}
	name := strings.TrimSpace(op)
	if name == "" {
		name = "database operation"
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		err := fn()
		if err == nil {
			return nil
		}
		if !IsDatabaseLocked(err) {
			return err
		}
		lastErr = err

		delay := databaseLockRetryDelay(attempt)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%s: database remained locked: %w (last error: %v)", name, ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
}

// RetryOnDatabaseLockedValue is RetryOnDatabaseLocked for functions that return a value.
func RetryOnDatabaseLockedValue[T any](ctx context.Context, op string, fn func() (T, error)) (T, error) {
	var zero T
	if fn == nil {
		return zero, fmt.Errorf("%s: nil operation", strings.TrimSpace(op))
	}
	var out T
	err := RetryOnDatabaseLocked(ctx, op, func() error {
		var opErr error
		out, opErr = fn()
		return opErr
	})
	if err != nil {
		return zero, err
	}
	return out, nil
}

func databaseLockRetryDelay(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	backoff := 25 * time.Millisecond
	for i := 0; i < attempt && i < 5; i++ {
		backoff *= 2
	}
	if backoff > 800*time.Millisecond {
		backoff = 800 * time.Millisecond
	}
	jitter := time.Duration((attempt%7)*15) * time.Millisecond
	return backoff + jitter
}

// isDuplicateColumnError checks if an error is a "duplicate column name" SQLite error.
func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column name")
}

// isNoSuchTable checks if an error is a "no such table" SQLite error.
func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table:")
}
