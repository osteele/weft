package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveSetupCommandNoneSkipsDetection(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname='x'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uv.lock"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := DetectSetupCommand(dir); got != "uv sync" {
		t.Fatalf("DetectSetupCommand = %q, want uv sync", got)
	}
	// A job that owns its environment never runs detected setup, even in a
	// directory that would otherwise trigger uv sync (wb129).
	if got := ResolveSetupCommand("none", dir); got != "" {
		t.Fatalf("ResolveSetupCommand(none) = %q, want empty", got)
	}
	if got := ResolveSetupCommand("", dir); got != "uv sync" {
		t.Fatalf("ResolveSetupCommand(auto) = %q, want uv sync", got)
	}
	if got := ResolveSetupCommand("auto", dir); got != "uv sync" {
		t.Fatalf("ResolveSetupCommand(auto) = %q, want uv sync", got)
	}
}
