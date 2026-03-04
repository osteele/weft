package services

import (
	"testing"

	"github.com/osteele/weft/internal/intent"
)

func TestRetryQueue(t *testing.T) {
	t.Run("Add and Len", func(t *testing.T) {
		q := NewRetryQueue()
		if q.Len() != 0 {
			t.Fatalf("new queue Len() = %d, want 0", q.Len())
		}
		q.Add(&intent.Intent{IntentID: "a"}, "host1")
		q.Add(&intent.Intent{IntentID: "b"}, "host2")
		q.Add(&intent.Intent{IntentID: "c"}, "host1")
		if q.Len() != 3 {
			t.Fatalf("Len() = %d, want 3", q.Len())
		}
	})

	t.Run("DrainForHost partitions correctly", func(t *testing.T) {
		q := NewRetryQueue()
		q.Add(&intent.Intent{IntentID: "a"}, "host1")
		q.Add(&intent.Intent{IntentID: "b"}, "host2")
		q.Add(&intent.Intent{IntentID: "c"}, "host1")

		drained := q.DrainForHost("host1")
		if len(drained) != 2 {
			t.Fatalf("DrainForHost(host1) returned %d items, want 2", len(drained))
		}
		if drained[0].Intent.IntentID != "a" || drained[1].Intent.IntentID != "c" {
			t.Errorf("drained items = %v, %v; want a, c", drained[0].Intent.IntentID, drained[1].Intent.IntentID)
		}
		if q.Len() != 1 {
			t.Errorf("remaining Len() = %d, want 1", q.Len())
		}
	})

	t.Run("DrainForHost no match returns nil", func(t *testing.T) {
		q := NewRetryQueue()
		q.Add(&intent.Intent{IntentID: "a"}, "host1")
		drained := q.DrainForHost("host99")
		if len(drained) != 0 {
			t.Errorf("DrainForHost(host99) returned %d items, want 0", len(drained))
		}
		if q.Len() != 1 {
			t.Errorf("Len() = %d, want 1", q.Len())
		}
	})
}

func TestIsSSHConnectionError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"non-connection error", errForTest("some random error"), false},
		{"connection timed out", errForTest("ssh: connect to host cool30 port 22: Operation timed out"), true},
		{"connection refused", errForTest("ssh: connect to host cool30 port 22: Connection refused"), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSSHConnectionError(tt.err)
			if got != tt.want {
				t.Errorf("isSSHConnectionError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

type errForTest string

func (e errForTest) Error() string { return string(e) }
