package sync

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestDefaultExcludes(t *testing.T) {
	excludes := DefaultExcludes()
	required := []string{".git", ".jj", ".venv", "__pycache__", "node_modules", ".DS_Store", "build", "dist", ".weft.toml", ".weft.yaml", "output", "outputs"}
	for _, pattern := range required {
		if !slices.Contains(excludes, pattern) {
			t.Errorf("DefaultExcludes() missing expected pattern %q", pattern)
		}
	}
}

func TestSyncSourcesExcludesCustomOutputDirs(t *testing.T) {
	// Create a temp dir with a .weft.toml that configures custom output dirs.
	tmpDir := t.TempDir()
	weftToml := filepath.Join(tmpDir, ".weft.toml")
	err := os.WriteFile(weftToml, []byte("[outputs]\ndirs = [\"results/\", \"data/processed/\"]\n[sync]\nexclude_dirs = [\"data/raw\", \"runs\"]\n"), 0644)
	if err != nil {
		t.Fatalf("write .weft.toml: %v", err)
	}

	// Capture the excludes passed to rsync
	var capturedExcludes []string
	cleanup := SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		capturedExcludes = excludes
		return nil
	})
	defer cleanup()

	if err := SyncSources("testhost", tmpDir, "~/remote"); err != nil {
		t.Fatalf("SyncSources: %v", err)
	}

	for _, want := range []string{"results", "data/processed", "data/raw", "runs", "output", "outputs"} {
		if !slices.Contains(capturedExcludes, want) {
			t.Errorf("excludes missing %q", want)
		}
	}
}

func TestBuildRsyncArgs(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		localDir  string
		remoteDir string
		excludes  []string
		wantFlags []string // substrings that must appear in the joined args
		wantSrc   string
		wantDst   string
	}{
		{
			name:      "basic",
			host:      "host-beta",
			localDir:  "/Users/osteele/code/research/compression-lab",
			remoteDir: "~/code/research/compression-lab",
			excludes:  []string{".git", "__pycache__"},
			wantFlags: []string{"-az", "--delete", "--exclude", ".git", "--exclude", "__pycache__"},
			wantSrc:   "/Users/osteele/code/research/compression-lab/",
			wantDst:   "host-beta:~/code/research/compression-lab/",
		},
		{
			name:      "trailing slash stripped",
			host:      "host-gamma",
			localDir:  "/home/user/project/",
			remoteDir: "~/project/",
			excludes:  nil,
			wantSrc:   "/home/user/project/",
			wantDst:   "host-gamma:~/project/",
		},
		{
			name:      "no excludes",
			host:      "host-alpha",
			localDir:  "/tmp/src",
			remoteDir: "~/src",
			excludes:  nil,
			wantSrc:   "/tmp/src/",
			wantDst:   "host-alpha:~/src/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := BuildRsyncArgs(tt.host, tt.localDir, tt.remoteDir, tt.excludes)

			// Check that args start with -az --delete
			if len(args) < 2 || args[0] != "-az" || args[1] != "--delete" {
				t.Errorf("args should start with -az --delete, got: %v", args[:min(4, len(args))])
			}

			// Check source and destination are last two args
			src := args[len(args)-2]
			dst := args[len(args)-1]
			if src != tt.wantSrc {
				t.Errorf("source = %q, want %q", src, tt.wantSrc)
			}
			if dst != tt.wantDst {
				t.Errorf("destination = %q, want %q", dst, tt.wantDst)
			}

			// Check expected flags are present
			joined := strings.Join(args, " ")
			for _, flag := range tt.wantFlags {
				if !strings.Contains(joined, flag) {
					t.Errorf("args missing expected flag %q in: %s", flag, joined)
				}
			}
		})
	}
}

func TestBuildExtraPathRsyncArgs(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		localDir  string
		remoteDir string
		wantSrc   string
		wantDst   string
	}{
		{
			name:      "tilde path",
			host:      "host-beta",
			localDir:  "/Users/osteele/sources/vidur/data",
			remoteDir: "~/sources/vidur/data",
			wantSrc:   "/Users/osteele/sources/vidur/data/",
			wantDst:   "host-beta:~/sources/vidur/data/",
		},
		{
			name:      "absolute path",
			host:      "host-alpha",
			localDir:  "/data/profiling",
			remoteDir: "/data/profiling",
			wantSrc:   "/data/profiling/",
			wantDst:   "host-alpha:/data/profiling/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := BuildExtraPathRsyncArgs(tt.host, tt.localDir, tt.remoteDir)

			// Must NOT contain --delete
			for _, arg := range args {
				if arg == "--delete" {
					t.Error("BuildExtraPathRsyncArgs should not use --delete")
				}
			}

			// Must start with -az
			if len(args) < 1 || args[0] != "-az" {
				t.Errorf("args should start with -az, got: %v", args)
			}

			// Check source and destination
			src := args[len(args)-2]
			dst := args[len(args)-1]
			if src != tt.wantSrc {
				t.Errorf("source = %q, want %q", src, tt.wantSrc)
			}
			if dst != tt.wantDst {
				t.Errorf("destination = %q, want %q", dst, tt.wantDst)
			}
		})
	}
}
