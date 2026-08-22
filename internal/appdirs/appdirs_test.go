package appdirs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_STATE_HOME", root)

	got, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "weft"); got != want {
		t.Fatalf("StateDir() = %q, want %q", got, want)
	}
}

func TestDataDir(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_DATA_HOME", root)

	got, err := DataDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "weft"); got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}
}

func TestRelativeOverrideIsIgnored(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("home directory unavailable")
	}
	t.Setenv("XDG_STATE_HOME", "relative/state")

	got, err := StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "state", "weft"); got != want {
		t.Fatalf("StateDir() = %q, want %q", got, want)
	}
}
