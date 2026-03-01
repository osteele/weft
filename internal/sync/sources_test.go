package sync

import (
	"slices"
	"strings"
	"testing"
)

func TestDefaultExcludes(t *testing.T) {
	excludes := DefaultExcludes()
	required := []string{".git", ".jj", ".venv", "__pycache__", "node_modules", ".DS_Store", "build", "dist"}
	for _, pattern := range required {
		if !slices.Contains(excludes, pattern) {
			t.Errorf("DefaultExcludes() missing expected pattern %q", pattern)
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
			host:      "cool30",
			localDir:  "/Users/osteele/code/research/compression-lab",
			remoteDir: "~/code/research/compression-lab",
			excludes:  []string{".git", "__pycache__"},
			wantFlags: []string{"-az", "--delete", "--exclude", ".git", "--exclude", "__pycache__"},
			wantSrc:   "/Users/osteele/code/research/compression-lab/",
			wantDst:   "cool30:~/code/research/compression-lab/",
		},
		{
			name:      "trailing slash stripped",
			host:      "studio",
			localDir:  "/home/user/project/",
			remoteDir: "~/project/",
			excludes:  nil,
			wantSrc:   "/home/user/project/",
			wantDst:   "studio:~/project/",
		},
		{
			name:      "no excludes",
			host:      "cool100",
			localDir:  "/tmp/src",
			remoteDir: "~/src",
			excludes:  nil,
			wantSrc:   "/tmp/src/",
			wantDst:   "cool100:~/src/",
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
