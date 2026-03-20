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
	required := []string{".git", ".jj", ".venv", "__pycache__", "node_modules", ".DS_Store", "build", "dist", ".weft.toml", ".weft.yaml", "output", "outputs", ".gocache", ".gomodcache"}
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

func TestGitignoreFiltersAlwaysHasPerDirRule(t *testing.T) {
	tmpDir := t.TempDir()
	filters := gitignoreFilters(tmpDir)

	// The per-directory .gitignore dir-merge rule should always be present
	found := false
	for i := 0; i+1 < len(filters); i += 2 {
		if filters[i] == "--filter" && filters[i+1] == ":- .gitignore" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("gitignoreFilters missing per-directory .gitignore rule, got: %v", filters)
	}
}

func TestGitignoreFiltersWithGitInfoExclude(t *testing.T) {
	tmpDir := t.TempDir()
	infoDir := filepath.Join(tmpDir, ".git", "info")
	if err := os.MkdirAll(infoDir, 0755); err != nil {
		t.Fatal(err)
	}
	excludeFile := filepath.Join(infoDir, "exclude")
	if err := os.WriteFile(excludeFile, []byte("# test\n"), 0644); err != nil {
		t.Fatal(err)
	}

	filters := gitignoreFilters(tmpDir)

	// Should contain a filter pointing to the exclude file
	found := false
	for i := 0; i+1 < len(filters); i += 2 {
		if filters[i] == "--filter" && filters[i+1] == ".- "+excludeFile {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("gitignoreFilters missing .git/info/exclude rule, got: %v", filters)
	}
}

func TestGitignoreFiltersNoGitDir(t *testing.T) {
	tmpDir := t.TempDir()
	filters := gitignoreFilters(tmpDir)

	for _, f := range filters {
		if strings.Contains(f, "info/exclude") {
			t.Errorf("gitignoreFilters should not reference info/exclude without .git dir, got: %v", filters)
		}
	}
}

func TestBuildRsyncArgsFiltersBeforeExcludes(t *testing.T) {
	tmpDir := t.TempDir()
	args := BuildRsyncArgsWithOptions("host", tmpDir, "~/remote", []string{".git"}, true)

	firstFilter := -1
	firstExclude := -1
	for i, arg := range args {
		if arg == "--filter" && firstFilter == -1 {
			firstFilter = i
		}
		if arg == "--exclude" && firstExclude == -1 {
			firstExclude = i
		}
	}

	if firstFilter == -1 {
		t.Fatal("no --filter found in rsync args")
	}
	if firstExclude == -1 {
		t.Fatal("no --exclude found in rsync args")
	}
	if firstFilter > firstExclude {
		t.Errorf("--filter (index %d) should come before --exclude (index %d)", firstFilter, firstExclude)
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
