package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/ssh"
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

			// Check that args contain -az and --delete
			joinedEarly := strings.Join(args, " ")
			if !strings.Contains(joinedEarly, "-az") || !strings.Contains(joinedEarly, "--delete") {
				t.Errorf("args should contain -az and --delete, got: %v", args[:min(6, len(args))])
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

func TestBuildRsyncArgsUsesConfiguredSSHUser(t *testing.T) {
	ssh.Configure(&config.Config{
		Hosts: map[string]config.HostConfig{
			"studio": {
				SSHUser:         "agent",
				SSHIdentityFile: "/path/to/agent_studio_ed25519",
			},
		},
	})
	t.Cleanup(func() { ssh.Configure(&config.Config{}) })

	args := BuildRsyncArgs("studio", "/tmp/src", "~/code/project", []string{".git"})
	dst := args[len(args)-1]
	if dst != "agent@studio:~/code/project/" {
		t.Fatalf("destination = %q, want agent@studio target", dst)
	}

	eIdx := slices.Index(args, "-e")
	if eIdx < 0 || eIdx+1 >= len(args) {
		t.Fatalf("missing rsync -e ssh command: %v", args)
	}
	sshCmd := args[eIdx+1]
	for _, want := range []string{"IdentityAgent=none", "IdentitiesOnly=yes", "-i /path/to/agent_studio_ed25519"} {
		if !strings.Contains(sshCmd, want) {
			t.Fatalf("rsync ssh command = %q, missing %q", sshCmd, want)
		}
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

func TestIsIgnorableRsyncError(t *testing.T) {
	tests := []struct {
		name   string
		script string
		stderr string
		want   bool
	}{
		{
			name:   "vanished file warning",
			script: "exit 24",
			stderr: "file has vanished",
			want:   true,
		},
		{
			name:   "non-vanished rsync failure",
			script: "exit 24",
			stderr: "permission denied",
			want:   false,
		},
		{
			name:   "different exit code",
			script: "exit 23",
			stderr: "file has vanished",
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := exec.Command("sh", "-c", tt.script).Run()
			if err == nil {
				t.Fatal("expected command to fail")
			}
			if got := isIgnorableRsyncError(err, tt.stderr); got != tt.want {
				t.Fatalf("isIgnorableRsyncError(...) = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseGitignorePatterns(t *testing.T) {
	tmpDir := t.TempDir()

	gitignore := `# Python
__pycache__/
*.py[cod]
*.so

# Training outputs
checkpoints/
*.pt
*.pth

# Negation (should be skipped)
!important.txt

# Path-based (should be skipped)
src/generated/output

# Data
data/
wandb/
`
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte(gitignore), 0644); err != nil {
		t.Fatal(err)
	}

	patterns := parseGitignorePatterns(tmpDir)

	want := []string{"__pycache__", "*.py[cod]", "*.so", "checkpoints", "*.pt", "*.pth", "data", "wandb"}
	for _, w := range want {
		if !slices.Contains(patterns, w) {
			t.Errorf("missing expected pattern %q, got: %v", w, patterns)
		}
	}

	// Negation and path-based patterns should NOT be included
	for _, bad := range []string{"!important.txt", "important.txt", "src/generated/output"} {
		if slices.Contains(patterns, bad) {
			t.Errorf("should not contain %q", bad)
		}
	}
}

func TestParseGitignorePatternsNoFile(t *testing.T) {
	tmpDir := t.TempDir()
	patterns := parseGitignorePatterns(tmpDir)
	if len(patterns) != 0 {
		t.Errorf("expected empty patterns for dir without .gitignore, got: %v", patterns)
	}
}

func TestSourceExcludesIncludesGitignore(t *testing.T) {
	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, ".gitignore"), []byte("checkpoints/\n*.pt\n"), 0644); err != nil {
		t.Fatal(err)
	}

	excludes := sourceExcludes(tmpDir)
	for _, want := range []string{"checkpoints", "*.pt"} {
		if !slices.Contains(excludes, want) {
			t.Errorf("sourceExcludes missing .gitignore pattern %q", want)
		}
	}
}

func TestBuildExtraPathRsyncArgs(t *testing.T) {
	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "calibration.db")
	if err := os.WriteFile(filePath, []byte("db"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
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
		{
			name:      "file path",
			host:      "host-alpha",
			localDir:  filePath,
			remoteDir: "~/code/project/data/calibration.db",
			wantSrc:   filePath,
			wantDst:   "host-alpha:~/code/project/data/calibration.db",
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

			// Must contain -az
			if !slices.Contains(args, "-az") {
				t.Errorf("args should contain -az, got: %v", args)
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

func TestSyncExtraPathsUsesExtraPathSyncFunc(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var capturedHost, capturedLocal, capturedRemote string
	var capturedExcludes []string
	cleanup := SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		capturedHost = host
		capturedLocal = localDir
		capturedRemote = remoteDir
		capturedExcludes = excludes
		return nil
	})
	defer cleanup()

	if err := SyncExtraPaths("host-beta", []string{"~/code/project/data"}); err != nil {
		t.Fatalf("SyncExtraPaths: %v", err)
	}
	if capturedHost != "host-beta" {
		t.Errorf("host = %q, want host-beta", capturedHost)
	}
	wantLocal := filepath.Join(home, "code", "project", "data")
	if capturedLocal != wantLocal {
		t.Errorf("local = %q, want %q", capturedLocal, wantLocal)
	}
	if capturedRemote != "~/code/project/data" {
		t.Errorf("remote = %q, want ~/code/project/data", capturedRemote)
	}
	if capturedExcludes != nil {
		t.Errorf("excludes = %v, want nil", capturedExcludes)
	}
}

func TestSyncSourcesToHostSyncsLocalFileInputAsProjectOverlay(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	localDir := filepath.Join(home, "code", "project")
	if err := os.MkdirAll(filepath.Join(localDir, "data"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "main.py"), []byte("print('ok')\n"), 0o644); err != nil {
		t.Fatalf("WriteFile main: %v", err)
	}
	filePath := filepath.Join(localDir, "data", "calibration.db")
	if err := os.WriteFile(filePath, []byte("db"), 0o644); err != nil {
		t.Fatalf("WriteFile db: %v", err)
	}

	type call struct {
		host, local, remote string
		excludesNil         bool
	}
	var calls []call
	cleanup := SetSyncFunc(func(host, localDir, remoteDir string, excludes []string) error {
		calls = append(calls, call{
			host:        host,
			local:       localDir,
			remote:      remoteDir,
			excludesNil: excludes == nil,
		})
		return nil
	})
	defer cleanup()

	if err := SyncSourcesToHost("host-beta", localDir, "~/remote/project", []string{"local:data/calibration.db"}); err != nil {
		t.Fatalf("SyncSourcesToHost: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("sync calls = %#v, want source sync plus local input overlay", calls)
	}
	if calls[0].local != localDir || calls[0].remote != "~/remote/project" || calls[0].excludesNil {
		t.Fatalf("source sync call = %#v", calls[0])
	}
	if calls[1].local != filePath || calls[1].remote != "~/remote/project/data/calibration.db" || !calls[1].excludesNil {
		t.Fatalf("local file overlay sync call = %#v", calls[1])
	}
}

func TestExtraPathRsyncTimeoutLongerThanSourceTimeout(t *testing.T) {
	if extraPathRsyncTimeout <= rsyncTimeout {
		t.Fatalf("extraPathRsyncTimeout = %s, want > %s", extraPathRsyncTimeout, rsyncTimeout)
	}
	if extraPathRsyncTimeout < 10*time.Minute {
		t.Fatalf("extraPathRsyncTimeout = %s, want at least 10m", extraPathRsyncTimeout)
	}
}

// TestRsyncArgsUseNonInteractiveSSH is a regression test: rsync args must pass
// "-e ssh -o BatchMode=yes ..." so ssh never prompts via /dev/tty, which would
// corrupt a TUI running in the same terminal and hang the process waiting for
// a yes/no answer.
func TestRsyncArgsUseNonInteractiveSSH(t *testing.T) {
	cases := map[string][]string{
		"BuildRsyncArgs":            BuildRsyncArgs("host", "/tmp/src", "~/dst", []string{".git"}),
		"BuildRsyncArgsWithOptions": BuildRsyncArgsWithOptions("host", "/tmp/src", "~/dst", nil, false),
		"BuildExtraPathRsyncArgs":   BuildExtraPathRsyncArgs("host", "/tmp/x", "~/x"),
		"BuildOutputFileSyncArgs":   BuildOutputFileSyncArgs("host", "~/remote", "/tmp/local"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			idx := slices.Index(args, "-e")
			if idx < 0 || idx+1 >= len(args) {
				t.Fatalf("%s: missing -e <ssh-command>: %v", name, args)
			}
			sshCmd := args[idx+1]
			if !strings.Contains(sshCmd, "BatchMode=yes") {
				t.Errorf("%s: -e value %q missing BatchMode=yes", name, sshCmd)
			}
		})
	}
}
