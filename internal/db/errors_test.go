package db

import (
	"context"
	"errors"
	"testing"
)

// busyErr returns an error that IsDatabaseLocked recognizes as SQLITE_BUSY.
func busyErr() error {
	return errors.New("database is locked (5) (SQLITE_BUSY)")
}

func TestRetryOnDatabaseLocked_RetriesTransientBusyThenSucceeds(t *testing.T) {
	attempts := 0
	err := RetryOnDatabaseLocked(context.Background(), "test write", func() error {
		attempts++
		if attempts <= 3 {
			return busyErr()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("RetryOnDatabaseLocked returned %v, want nil", err)
	}
	if attempts != 4 {
		t.Fatalf("attempts = %d, want 4 (3 busy + 1 success)", attempts)
	}
}

func TestRetryOnDatabaseLockedValue_RetriesTransientBusyThenSucceeds(t *testing.T) {
	attempts := 0
	got, err := RetryOnDatabaseLockedValue(context.Background(), "test write", func() (int64, error) {
		attempts++
		if attempts <= 2 {
			return 0, busyErr()
		}
		return 4242, nil
	})
	if err != nil {
		t.Fatalf("RetryOnDatabaseLockedValue returned err %v, want nil", err)
	}
	if got != 4242 {
		t.Fatalf("value = %d, want 4242", got)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (2 busy + 1 success)", attempts)
	}
}

func TestRetryOnDatabaseLocked_NonLockErrorNotRetried(t *testing.T) {
	sentinel := errors.New("invalid status transition")
	attempts := 0
	err := RetryOnDatabaseLocked(context.Background(), "test write", func() error {
		attempts++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel returned immediately", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (non-lock errors must not be retried)", attempts)
	}
}

func TestIsDatabaseReadOnly_MatchesKnownMessages(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "readonly message",
			err:  errors.New("attempt to write a readonly database (8)"),
			want: true,
		},
		{
			name: "sqlite readonly token",
			err:  errors.New("SQLITE_READONLY: database is read only"),
			want: true,
		},
		{
			name: "wrapped readonly message",
			err:  errors.New("startup repair: close duplicate open attempts: attempt to write a readonly database (8)"),
			want: true,
		},
		{
			name: "unrelated message",
			err:  errors.New("database is locked"),
			want: false,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := IsDatabaseReadOnly(tc.err); got != tc.want {
				t.Fatalf("IsDatabaseReadOnly(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
