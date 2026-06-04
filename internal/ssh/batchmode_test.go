package ssh

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBatchModeArgs(t *testing.T) {
	args := BatchModeArgs(10*time.Second, "ConnectionAttempts=1")
	want := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ConnectionAttempts=1"}
	if !slices.Equal(args, want) {
		t.Fatalf("BatchModeArgs = %v, want %v", args, want)
	}
}

func TestBatchModeArgs_SubSecondTimeoutFloorsToOne(t *testing.T) {
	args := BatchModeArgs(500 * time.Millisecond)
	if !slices.Contains(args, "ConnectTimeout=1") {
		t.Fatalf("expected ConnectTimeout=1 for sub-second timeout, got %v", args)
	}
}

func TestBatchModeRsyncCommand(t *testing.T) {
	got := BatchModeRsyncCommand(15*time.Second, "ServerAliveInterval=30")
	want := "ssh -o BatchMode=yes -o ConnectTimeout=15 -o ServerAliveInterval=30"
	if got != want {
		t.Fatalf("BatchModeRsyncCommand = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "ssh ") {
		t.Errorf("rsync command should start with 'ssh ', got %q", got)
	}
}

func TestBatchModeRsyncCommandForHostUsesConfiguredIdentity(t *testing.T) {
	resetIdentityGlobals(t)
	sshUserByHost = map[string]string{"studio": "agent"}
	sshIdentityByHost = map[string]string{"studio": "/path/to/agent key"}

	got := BatchModeRsyncCommandForHost("studio", 15*time.Second, "ConnectionAttempts=1")
	for _, want := range []string{
		"-o BatchMode=yes",
		"-o ConnectTimeout=15",
		"-o ConnectionAttempts=1",
		"-o IdentityAgent=none",
		"-o IdentitiesOnly=yes",
		"-i '/path/to/agent key'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("BatchModeRsyncCommandForHost = %q, missing %q", got, want)
		}
	}
}

func TestRsyncTargetUsesConfiguredUser(t *testing.T) {
	resetIdentityGlobals(t)
	sshUserByHost = map[string]string{"studio": "agent"}

	if got := RsyncTarget("studio"); got != "agent@studio" {
		t.Fatalf("RsyncTarget = %q, want %q", got, "agent@studio")
	}
	if got := RsyncTarget("cool30"); got != "cool30" {
		t.Fatalf("RsyncTarget unconfigured = %q, want %q", got, "cool30")
	}
}

func TestScpTargetUsesConfiguredUser(t *testing.T) {
	resetIdentityGlobals(t)
	sshUserByHost = map[string]string{"studio": "agent"}

	if got := scpTarget("studio", "~/.cache/weft/bin/notify-slack.sh"); got != "agent@studio:~/.cache/weft/bin/notify-slack.sh" {
		t.Fatalf("scpTarget = %q, want %q", got, "agent@studio:~/.cache/weft/bin/notify-slack.sh")
	}
}
