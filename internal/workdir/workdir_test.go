package workdir

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectName(t *testing.T) {
	original := repoRootResolver
	repoRootResolver = func(string) string { return "" }
	t.Cleanup(func() { repoRootResolver = original })

	tests := []struct {
		dir  string
		want string
	}{
		{"~/code/research/llm-performance-models", "llm-performance-models"},
		{"/home/user/project", "project"},
		{"~/single", "single"},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			if got := ProjectName(tt.dir); got != tt.want {
				t.Errorf("ProjectName(%q) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestProjectNameEmpty(t *testing.T) {
	original := repoRootResolver
	repoRootResolver = func(string) string { return "" }
	t.Cleanup(func() { repoRootResolver = original })

	cwd, err := os.Getwd()
	if err != nil {
		t.Skip("cannot get cwd")
	}
	got := ProjectName("")
	want := filepath.Base(cwd)
	if got != want {
		t.Errorf("ProjectName(\"\") = %q, want %q", got, want)
	}
}

func TestProjectNameUsesRepoRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo-root")
	child := filepath.Join(root, "pkg", "subdir")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}

	original := repoRootResolver
	repoRootResolver = func(dir string) string {
		if dir == child {
			return root
		}
		return ""
	}
	t.Cleanup(func() { repoRootResolver = original })

	got := ProjectName(child)
	if got != "repo-root" {
		t.Errorf("ProjectName(%q) = %q, want %q", child, got, "repo-root")
	}
}

func TestResolveProjectNameExplicitOverride(t *testing.T) {
	got, err := ResolveProjectName("exp-123", "")
	if err != nil {
		t.Fatalf("ResolveProjectName: %v", err)
	}
	if got != "exp-123" {
		t.Errorf("ResolveProjectName explicit = %q, want %q", got, "exp-123")
	}
}

func TestResolveLocal(t *testing.T) {
	tests := []struct {
		name string
		dir  string
	}{
		{"empty returns empty", ""},
		{"absolute path returns itself", "/tmp/test"},
		{"relative non-tilde returns empty", "relative/path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveLocal(tt.dir)
			switch tt.name {
			case "empty returns empty":
				if got != "" {
					t.Errorf("ResolveLocal(%q) = %q, want empty", tt.dir, got)
				}
			case "absolute path returns itself":
				if got != tt.dir {
					t.Errorf("ResolveLocal(%q) = %q, want %q", tt.dir, got, tt.dir)
				}
			case "relative non-tilde returns empty":
				if got != "" {
					t.Errorf("ResolveLocal(%q) = %q, want empty", tt.dir, got)
				}
			}
		})
	}
}
