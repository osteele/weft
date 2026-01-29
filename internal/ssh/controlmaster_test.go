package ssh

import (
	"strings"
	"testing"
)

func TestSSHCommandArgOrder(t *testing.T) {
	cmd := sshCommand("argtest-host", "ls -la", "-o", "BatchMode=yes")
	args := cmd.Args

	if args[0] != "ssh" {
		t.Errorf("args[0] = %q, want ssh", args[0])
	}

	// Host should be second-to-last, command last
	if args[len(args)-2] != "argtest-host" {
		t.Errorf("host should be second-to-last arg, got %q in %v", args[len(args)-2], args)
	}
	if args[len(args)-1] != "ls -la" {
		t.Errorf("command should be last arg, got %q", args[len(args)-1])
	}

	// Extra args should appear before host
	joined := strings.Join(args, " ")
	batchIdx := strings.Index(joined, "BatchMode=yes")
	hostIdx := strings.Index(joined, "argtest-host")
	if batchIdx > hostIdx {
		t.Error("extra args should appear before host")
	}
}

func TestSCPCommandArgOrder(t *testing.T) {
	cmd := scpCommand("scptest-host", "local.txt", "scptest-host:remote.txt")
	args := cmd.Args

	if args[0] != "scp" {
		t.Errorf("args[0] = %q, want scp", args[0])
	}

	// -q should be first flag
	if args[1] != "-q" {
		t.Errorf("args[1] = %q, want -q", args[1])
	}

	// SCP args should be last
	if args[len(args)-2] != "local.txt" {
		t.Errorf("source should be second-to-last, got %q", args[len(args)-2])
	}
	if args[len(args)-1] != "scptest-host:remote.txt" {
		t.Errorf("dest should be last, got %q", args[len(args)-1])
	}
}
