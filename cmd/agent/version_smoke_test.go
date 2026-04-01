//go:build smoke
// +build smoke

package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow build test in short mode")
	}

	tmpDir := t.TempDir()
	bin := filepath.Join(tmpDir, "weft-agent")

	cmd := exec.Command("go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	root := filepath.Dir(strings.TrimSpace(string(out)))

	buildCmd := exec.Command("go", "build", "-ldflags", "-X main.version=test123", "-o", bin, "./cmd/agent")
	buildCmd.Dir = root
	if output, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("build agent: %v\n%s", err, output)
	}

	runCmd := exec.Command(bin, "--version")
	output, err := runCmd.Output()
	if err != nil {
		t.Fatalf("run agent --version: %v", err)
	}

	got := strings.TrimSpace(string(output))
	if got != "weft-agent test123" {
		t.Errorf("expected 'weft-agent test123', got %q", got)
	}
}
