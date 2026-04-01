package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFakeRclone(t *testing.T, dir string) string {
	t.Helper()

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake rclone bin dir: %v", err)
	}

	script := `#!/bin/sh
if [ "$1" != "cat" ]; then
  echo "unexpected command: $*" >&2
  exit 1
fi

case "$2" in
  r2:test-bucket/missing)
    echo "ERROR : object not found" >&2
    exit 1
    ;;
  r2:test-bucket/broken)
    echo "permission denied" >&2
    exit 1
    ;;
  r2:test-bucket/value)
    printf 'hello world\n'
    ;;
  *)
    echo "unexpected path: $2" >&2
    exit 1
    ;;
esac
`
	path := filepath.Join(binDir, "rclone")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake rclone: %v", err)
	}
	return binDir
}

func TestR2Get_MissingKeyReturnsEmpty(t *testing.T) {
	fakeBinDir := writeFakeRclone(t, t.TempDir())
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := r2Get("test-bucket", "missing")
	if err != nil {
		t.Fatalf("r2Get missing: %v", err)
	}
	if got != "" {
		t.Fatalf("r2Get missing = %q, want empty", got)
	}
}

func TestR2Get_PropagatesNonMissingErrors(t *testing.T) {
	fakeBinDir := writeFakeRclone(t, t.TempDir())
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := r2Get("test-bucket", "broken")
	if err == nil {
		t.Fatal("expected error for non-missing r2 failure")
	}
	if got != "" {
		t.Fatalf("r2Get broken = %q, want empty", got)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v, want permission denied", err)
	}
}
