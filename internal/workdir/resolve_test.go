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
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot get home dir")
	}

	// If ~/code doesn't exist, skip.
	codeDir := filepath.Join(home, "code")
	if _, err := os.Stat(codeDir); os.IsNotExist(err) {
		t.Skip("~/code does not exist")
	}

	t.Chdir(codeDir)

	var buf bytes.Buffer
	dir, err := ResolveWorkingDir("", &buf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, "~/code") {
		t.Errorf("expected automap result starting with ~/code, got %q", dir)
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
