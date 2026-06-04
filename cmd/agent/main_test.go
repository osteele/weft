package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/agentenv"
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

func TestOpsLogInitialization(t *testing.T) {
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "test-agent-ops.log")

	if err := oplog.Init(logPath, 0); err != nil {
		t.Fatalf("oplog.Init: %v", err)
	}

	oplog.Log(oplog.OpAgentStart, oplog.WithDetail("test-version"))
	oplog.Log(oplog.OpAgentStop, oplog.WithDetail("signal: SIGTERM"))
	oplog.Close()

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
	origHome := os.Getenv("HOME")
	defer os.Setenv("HOME", origHome)

	os.Setenv("HOME", "/custom/home")
	path := agentLogPath()
	if !strings.HasSuffix(path, "agent-operations.log") {
		t.Errorf("expected path to end with 'agent-operations.log', got %s", path)
	}
}

func TestEnsureUserToolPath(t *testing.T) {
	t.Setenv("HOME", "/Users/agent")
	t.Setenv("PATH", "/usr/bin:/bin")

	agentenv.EnsureToolPath()

	got := os.Getenv("PATH")
	wantPrefix := "/Users/agent/.cache/weft/bin:/Users/agent/.local/bin:/Users/agent/bin:/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/local/sbin:/usr/bin:/bin"
	if got != wantPrefix {
		t.Fatalf("PATH = %q, want %q", got, wantPrefix)
	}
}

func TestParseRunQueueArgs(t *testing.T) {
	t.Run("accepts no queue name", func(t *testing.T) {
		args, err := parseRunQueueArgs(nil)
		if err != nil {
			t.Fatalf("parseRunQueueArgs() error = %v", err)
		}
		if args.R2Bucket != "" {
			t.Fatalf("parseRunQueueArgs() r2Bucket = %q, want empty", args.R2Bucket)
		}
	})

	t.Run("accepts explicit default queue for compatibility", func(t *testing.T) {
		args, err := parseRunQueueArgs([]string{"default", "--r2-bucket=test-bucket"})
		if err != nil {
			t.Fatalf("parseRunQueueArgs() error = %v", err)
		}
		if args.R2Bucket != "test-bucket" {
			t.Fatalf("parseRunQueueArgs() r2Bucket = %q, want test-bucket", args.R2Bucket)
		}
	})

	t.Run("rejects non-default queue", func(t *testing.T) {
		if _, err := parseRunQueueArgs([]string{"gpu"}); err == nil {
			t.Fatal("parseRunQueueArgs() error = nil, want error")
		}
	})
}
