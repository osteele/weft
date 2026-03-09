package sync

import (
	"fmt"
	"os"
	"testing"
)

func TestIsLocalHost(t *testing.T) {
	hostname, _ := os.Hostname()

	tests := []struct {
		host string
		want bool
	}{
		{"", true},
		{"localhost", true},
		{"127.0.0.1", true},
		{"::1", true},
		{hostname, true},
		{"host-beta", false},
		{"host-alpha", false},
		{"host-gamma", false},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			if got := IsLocalHost(tt.host); got != tt.want {
				t.Errorf("IsLocalHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestSyncSourcesToHostSkipsLocalHost(t *testing.T) {
	hostname, _ := os.Hostname()

	synced := false
	cleanup := SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		synced = true
		return nil
	})
	defer cleanup()

	// Should skip sync when host is localhost
	if err := SyncSourcesToHost(hostname, "/tmp/test", "~/test", nil); err != nil {
		t.Errorf("SyncSourcesToHost for localhost returned unexpected error: %v", err)
	}
	if synced {
		t.Error("SyncSourcesToHost should skip sync when target is local host")
	}

	// Should sync when host is remote
	if err := SyncSourcesToHost("host-beta", "/tmp/test", "~/test", nil); err != nil {
		t.Errorf("SyncSourcesToHost for remote host returned unexpected error: %v", err)
	}
	if !synced {
		t.Error("SyncSourcesToHost should sync when target is remote host")
	}
}

func TestSyncSourcesToHostReturnsError(t *testing.T) {
	cleanup := SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		return fmt.Errorf("connection timeout")
	})
	defer cleanup()

	err := SyncSourcesToHost("host-beta", "/tmp/test", "~/test", nil)
	if err == nil {
		t.Error("SyncSourcesToHost should return error when sync fails")
	}
}

func TestSyncSourcesToHostEmptyLocalDir(t *testing.T) {
	err := SyncSourcesToHost("host-beta", "", "~/test", nil)
	if err != nil {
		t.Errorf("SyncSourcesToHost with empty localDir should return nil, got: %v", err)
	}
}
