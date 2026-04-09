package campaign

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/estimate"
)

func TestMatchGroupToInstance_AccountsForCachedUVSyncDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	localDir := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("mkdir local dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "uv.lock"), []byte("lock\n"), 0o644); err != nil {
		t.Fatalf("write uv.lock: %v", err)
	}

	writeCachedUVManifest(t, home, localDir, &estimate.UVManifestRef{
		LockfileHash: estimate.LockfileHash([]string{localDir})[localDir],
		Platform:     "linux-amd64",
		CollectedAt:  time.Now().UTC(),
		Packages: []estimate.UVPackageRef{
			{Name: "torch", Version: "1.0.0", InstalledBytes: 12_000_000_000},
		},
	})

	group := InstanceGroup{
		Jobs: []*db.Job{
			{ID: 1, WorkingDir: localDir},
		},
	}
	cap := InstanceCapacity{
		Instance:   &db.Launch{DiskGB: 100, GPUMemGB: 24},
		DiskFreeGB: 10,
	}

	ok, reason := MatchGroupToInstance(group, cap)
	if ok {
		t.Fatal("MatchGroupToInstance unexpectedly accepted instance with insufficient disk for uv sync")
	}
	if reason != "disk insufficient: need=12GB free=10GB" {
		t.Fatalf("reason = %q, want disk insufficiency from uv sync", reason)
	}
}

func TestMatchGroupToInstance_RejectsGroupRequiringVastCapAdd(t *testing.T) {
	group := InstanceGroup{
		VastCapAdd: []string{"SYS_ADMIN"},
		Jobs:       []*db.Job{{ID: 1}},
	}
	cap := InstanceCapacity{
		Instance: &db.Launch{DiskGB: 100, GPUMemGB: 24},
	}

	ok, reason := MatchGroupToInstance(group, cap)
	if ok {
		t.Fatal("MatchGroupToInstance unexpectedly accepted group requiring vast-cap-add")
	}
	if reason != "requires fresh launch with vast-cap-add" {
		t.Fatalf("reason = %q, want capability guard", reason)
	}
}
