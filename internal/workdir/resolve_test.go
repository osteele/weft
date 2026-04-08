package workdir

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveWorkingDir_ExplicitDir(t *testing.T) {
	dir, err := ResolveWorkingDir("~/code/myproject", nil)
	if err != nil {
		t.Fatal(err)
	}
	if dir != "~/code/myproject" {
		t.Errorf("got %q, want ~/code/myproject", dir)
	}
}

func TestResolveWorkingDir_ExplicitRelativeDirBecomesAbsolute(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "project")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	t.Chdir(root)

	dir, err := ResolveWorkingDir("./project", nil)
	if err != nil {
		t.Fatal(err)
	}
	if dir != child {
		t.Errorf("got %q, want %q", dir, child)
	}
}

func TestResolveWorkingDir_ExplicitDirNoLog(t *testing.T) {
	var buf bytes.Buffer
	dir, err := ResolveWorkingDir("~/code/myproject", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if dir != "~/code/myproject" {
		t.Errorf("got %q, want ~/code/myproject", dir)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no log output for explicit dir, got %q", buf.String())
	}
}

func TestResolveWorkingDir_Automap(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)

	autoDir := filepath.Join(tmpHome, "structural-probes")
	if err := os.MkdirAll(autoDir, 0o755); err != nil {
		t.Fatalf("mkdir autoDir: %v", err)
	}

	t.Chdir(autoDir)

	var buf bytes.Buffer
	dir, err := ResolveWorkingDir("", &buf)
	if err != nil {
		t.Fatal(err)
	}
	expected := "~/structural-probes"
	if dir != expected {
		t.Errorf("expected automap result %q, got %q", expected, dir)
	}
	if !strings.Contains(buf.String(), "Auto-detected") {
		t.Errorf("expected 'Auto-detected' in log output, got %q", buf.String())
	}
}

func TestResolveWorkingDir_DefaultEmpty(t *testing.T) {
	t.Chdir(t.TempDir())

	dir, err := ResolveWorkingDir("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if dir != "" {
		t.Errorf("expected empty string for non-automap dir, got %q", dir)
	}
}

func TestNormalize_RejectsContainerPath(t *testing.T) {
	_, err := Normalize("/workspace/llm-performance-models")
	if err == nil {
		t.Fatal("expected error for container path, got nil")
	}
	if !strings.Contains(err.Error(), "container path") {
		t.Errorf("expected 'container path' in error, got %q", err.Error())
	}
}

func TestNormalize_AcceptsExistingAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	got, err := Normalize(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != dir {
		t.Errorf("got %q, want %q", got, dir)
	}
}

func TestNormalize_AcceptsTildePath(t *testing.T) {
	got, err := Normalize("~/code/myproject")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "~/code/myproject" {
		t.Errorf("got %q, want ~/code/myproject", got)
	}
}
