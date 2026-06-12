package syncorch

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/osteele/weft/internal/ops"
)

func withHostSyncFn(t *testing.T, fn func(*sql.DB, string, time.Duration, time.Duration, bool, func(string) (bool, error)) (ops.HostSyncResult, error)) {
	t.Helper()
	prev := hostSyncFn
	hostSyncFn = fn
	t.Cleanup(func() { hostSyncFn = prev })
}

func TestSyncHosts_ConnectionErrorClassifiesAsUnreachable(t *testing.T) {
	withHostSyncFn(t, func(_ *sql.DB, host string, _, _ time.Duration, _ bool, _ func(string) (bool, error)) (ops.HostSyncResult, error) {
		return ops.HostSyncResult{}, errors.New("ssh: connect to host " + host + " port 22: Operation timed out")
	})

	res := SyncHosts(nil, SyncOptions{Hosts: []string{"alpha"}, HostTimeout: 5 * time.Second})

	if got, want := res.Unreachable, []string{"alpha"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Unreachable = %v, want %v", got, want)
	}
	if len(res.Slow) != 0 {
		t.Errorf("Slow = %v, want empty", res.Slow)
	}
	if res.Completed {
		t.Errorf("Completed = true, want false")
	}
}

func TestSyncHosts_NonConnectionErrorClassifiesAsSlow(t *testing.T) {
	withHostSyncFn(t, func(_ *sql.DB, _ string, _, _ time.Duration, _ bool, _ func(string) (bool, error)) (ops.HostSyncResult, error) {
		return ops.HostSyncResult{}, errors.New("parse output: unexpected token")
	})

	res := SyncHosts(nil, SyncOptions{Hosts: []string{"beta"}, HostTimeout: 5 * time.Second})

	if got, want := res.Slow, []string{"beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Slow = %v, want %v", got, want)
	}
	if len(res.Unreachable) != 0 {
		t.Errorf("Unreachable = %v, want empty", res.Unreachable)
	}
	if res.Completed {
		t.Errorf("Completed = true, want false")
	}
}

func TestSyncHosts_DeadlineClassifiesAsSlow(t *testing.T) {
	withHostSyncFn(t, func(_ *sql.DB, _ string, _, _ time.Duration, _ bool, _ func(string) (bool, error)) (ops.HostSyncResult, error) {
		time.Sleep(200 * time.Millisecond)
		return ops.HostSyncResult{}, nil
	})

	res := SyncHosts(nil, SyncOptions{Hosts: []string{"gamma"}, HostTimeout: 50 * time.Millisecond})

	if got, want := res.Slow, []string{"gamma"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Slow = %v, want %v", got, want)
	}
	if len(res.Unreachable) != 0 {
		t.Errorf("Unreachable = %v, want empty", res.Unreachable)
	}
	if res.Completed {
		t.Errorf("Completed = true, want false")
	}
}

func TestSyncHosts_SuccessClassifiesAsReached(t *testing.T) {
	withHostSyncFn(t, func(_ *sql.DB, _ string, _, _ time.Duration, _ bool, _ func(string) (bool, error)) (ops.HostSyncResult, error) {
		return ops.HostSyncResult{Updated: 3}, nil
	})

	res := SyncHosts(nil, SyncOptions{Hosts: []string{"delta"}, HostTimeout: 5 * time.Second})

	if !res.Completed {
		t.Errorf("Completed = false, want true")
	}
	if res.Reached != 1 {
		t.Errorf("Reached = %d, want 1", res.Reached)
	}
	if res.Updated != 3 {
		t.Errorf("Updated = %d, want 3", res.Updated)
	}
	if len(res.Unreachable)+len(res.Slow) != 0 {
		t.Errorf("Unreachable=%v Slow=%v, both want empty", res.Unreachable, res.Slow)
	}
}

func TestSyncHostsPassesHostTimeoutAsSourceTimeout(t *testing.T) {
	var gotSSHTimeout, gotSourceTimeout time.Duration
	withHostSyncFn(t, func(_ *sql.DB, _ string, sshTimeout, sourceTimeout time.Duration, _ bool, _ func(string) (bool, error)) (ops.HostSyncResult, error) {
		gotSSHTimeout = sshTimeout
		gotSourceTimeout = sourceTimeout
		return ops.HostSyncResult{}, nil
	})

	res := SyncHosts(nil, SyncOptions{
		Hosts:       []string{"epsilon"},
		SSHTimeout:  5 * time.Second,
		HostTimeout: 10 * time.Minute,
	})

	if !res.Completed {
		t.Fatalf("Completed = false, want true")
	}
	if gotSSHTimeout != 5*time.Second {
		t.Fatalf("ssh timeout = %s, want 5s", gotSSHTimeout)
	}
	if gotSourceTimeout != 10*time.Minute {
		t.Fatalf("source timeout = %s, want 10m", gotSourceTimeout)
	}
}
