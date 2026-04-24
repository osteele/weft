package db

import (
	"errors"
	"testing"
)

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
