package sync

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollectExtraPaths_RelativeResolution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	localDir := filepath.Join(home, "code", "research", "structural-probes")
	if err := os.MkdirAll(filepath.Join(localDir, "data"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "data", "calibration.db"), []byte("db"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(localDir, "cache"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "external", "data"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(home, "shared", "models"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll("/tmp/weft-extras-test-data", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll("/tmp/weft-extras-test-data") })

	tests := []struct {
		name     string
		inputs   []string
		localDir string
		want     []string
	}{
		{
			name:     "relative path resolved to tilde-relative",
			inputs:   []string{"local:data/"},
			localDir: localDir,
			want:     []string{"~/code/research/structural-probes/data"},
		},
		{
			name:     "relative file path resolved to tilde-relative",
			inputs:   []string{"local:data/calibration.db"},
			localDir: localDir,
			want:     []string{"~/code/research/structural-probes/data/calibration.db"},
		},
		{
			name:     "tilde path passed through",
			inputs:   []string{"~/external/data/"},
			localDir: localDir,
			want:     []string{"~/external/data/"},
		},
		{
			name:     "absolute path passed through",
			inputs:   []string{"/tmp/weft-extras-test-data"},
			localDir: localDir,
			want:     []string{"/tmp/weft-extras-test-data"},
		},
		{
			name:     "asset refs excluded",
			inputs:   []string{"hf:gpt2", "local:cache/"},
			localDir: localDir,
			want:     []string{"~/code/research/structural-probes/cache"},
		},
		{
			name:     "mixed inputs",
			inputs:   []string{"local:data/", "hf:gpt2", "~/shared/models/"},
			localDir: localDir,
			want:     []string{"~/code/research/structural-probes/data", "~/shared/models/"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CollectExtraPaths(tt.inputs, tt.localDir)
			if len(got) != len(tt.want) {
				t.Fatalf("CollectExtraPaths() returned %d paths, want %d: %v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("path[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestLocalInputOverlays_FileAndDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	localDir := filepath.Join(home, "code", "project")
	if err := os.MkdirAll(filepath.Join(localDir, "data", "conllu"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "data", "calibration.db"), []byte("db"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := LocalInputOverlays(localDir, []string{
		"local:data/calibration.db",
		"local:data/conllu/",
	}, RequireLocalInput)
	if err != nil {
		t.Fatalf("LocalInputOverlays: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LocalInputOverlays returned %d overlays: %v", len(got), got)
	}
	if got[0].Rel != "data/calibration.db" || got[0].IsDir {
		t.Fatalf("file overlay = %+v, want rel data/calibration.db and IsDir=false", got[0])
	}
	if got[1].Rel != "data/conllu" || !got[1].IsDir {
		t.Fatalf("directory overlay = %+v, want rel data/conllu and IsDir=true", got[1])
	}
}

// Regression: local: inputs that name artifacts produced on a remote host
// should be skipped (not fed to rsync) when the path is absent locally.
// Triggered by wj1547: three local:runs/... paths that lived only on cool30
// caused rsync to exit 23 every host-sync pass, blocking dispatch indefinitely.
func TestCollectExtraPaths_AbsentLocalPathsAreSkipped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	localDir := filepath.Join(home, "code", "project")
	if err := os.MkdirAll(filepath.Join(localDir, "data"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	inputs := []string{
		"local:data/",                         // exists locally — keep
		"local:runs/produced-on-host-only/",   // absent locally — drop
		"local:runs/another-remote-artifact/", // absent locally — drop
		"hf:bert-base-cased",                  // asset ref — ignored by CollectExtraPaths
	}
	got := CollectExtraPaths(inputs, localDir)

	want := []string{"~/code/project/data"}
	if len(got) != len(want) {
		t.Fatalf("CollectExtraPaths() = %v, want %v", got, want)
	}
	if got[0] != want[0] {
		t.Errorf("path[0] = %q, want %q", got[0], want[0])
	}
}

func TestCollectExtraPaths_AbsentTildePathSkipped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	localDir := filepath.Join(home, "project")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	got := CollectExtraPaths([]string{"~/does-not-exist/"}, localDir)
	if len(got) != 0 {
		t.Fatalf("CollectExtraPaths() = %v, want []", got)
	}
}

func TestCollectExtraPaths_AbsentAbsolutePathSkipped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	localDir := filepath.Join(home, "project")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	got := CollectExtraPaths([]string{"/tmp/weft-definitely-not-here-7e9d2"}, localDir)
	if len(got) != 0 {
		t.Fatalf("CollectExtraPaths() = %v, want []", got)
	}
}
