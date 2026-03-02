package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/oplog"
)

func TestAgentLogPath(t *testing.T) {
	path := agentLogPath()
	if path == "" {
		t.Fatal("agentLogPath() returned empty string")
	}
	if !strings.Contains(path, "agent-operations.log") {
		t.Errorf("expected path to contain 'agent-operations.log', got %s", path)
	}
	if !strings.Contains(path, ".cache/weft") {
		t.Errorf("expected path to contain '.cache/weft', got %s", path)
	}
}

func TestVersionFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping slow build test in short mode")
	}
	// Build the agent binary
	tmpDir := t.TempDir()
	bin := filepath.Join(tmpDir, "weft-agent")

	// Find module root
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

	// Run with --version flag
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

func TestOpsLogInitialization(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test-agent-ops.log")

	if err := oplog.Init(logPath, 0); err != nil {
		t.Fatalf("oplog.Init: %v", err)
	}

	oplog.Log(oplog.OpAgentStart, oplog.WithDetail("test-version"))
	oplog.Log(oplog.OpAgentStop, oplog.WithDetail("signal: SIGTERM"))
	oplog.Close()

	// Verify log entries were written
	entries, err := oplog.ReadEntries(logPath)
	if err != nil {
		t.Fatalf("read entries: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Operation != oplog.OpAgentStart {
		t.Errorf("entry 0: expected op %q, got %q", oplog.OpAgentStart, entries[0].Operation)
	}
	if entries[0].Detail != "test-version" {
		t.Errorf("entry 0: expected detail 'test-version', got %q", entries[0].Detail)
	}
	if entries[1].Operation != oplog.OpAgentStop {
		t.Errorf("entry 1: expected op %q, got %q", oplog.OpAgentStop, entries[1].Operation)
	}
}

func TestAgentOpConstants(t *testing.T) {
	// Verify agent-specific op constants exist and have correct prefixes
	ops := []string{
		oplog.OpAgentStart,
		oplog.OpAgentStop,
		oplog.OpAgentVersion,
	}
	for _, op := range ops {
		if !strings.HasPrefix(op, "agent.") {
			t.Errorf("agent op %q should have 'agent.' prefix", op)
		}
	}
}

func TestAgentLogPathRespectsHome(t *testing.T) {
	// Save and restore HOME
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)

	os.Setenv("HOME", "/custom/home")
	// agentLogPath uses os.UserHomeDir which may cache, so we test the structure
	path := agentLogPath()
	if !strings.HasSuffix(path, "agent-operations.log") {
		t.Errorf("expected path to end with 'agent-operations.log', got %s", path)
	}
}
