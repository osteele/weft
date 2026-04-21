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
