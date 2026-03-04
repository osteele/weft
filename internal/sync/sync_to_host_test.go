package sync

import (
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
		{"cool30", false},
		{"cool100", false},
		{"studio", false},
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
	SyncSourcesToHost(hostname, "/tmp/test", "~/test", nil)
	if synced {
		t.Error("SyncSourcesToHost should skip sync when target is local host")
	}

	// Should sync when host is remote
	SyncSourcesToHost("cool30", "/tmp/test", "~/test", nil)
	if !synced {
		t.Error("SyncSourcesToHost should sync when target is remote host")
	}
}
