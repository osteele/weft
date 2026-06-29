package agentdeploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestShouldReclaimBuildLockKeepsOldLivePID(t *testing.T) {
	lockFile := filepath.Join(t.TempDir(), "weft-agent.lock")
	if err := os.WriteFile(lockFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}
	old := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(lockFile, old, old); err != nil {
		t.Fatalf("age lock: %v", err)
	}

	if shouldReclaimBuildLock(lockFile) {
		t.Fatal("old lock with live PID should not be reclaimed")
	}
}

func TestShouldReclaimBuildLockReclaimsDeadPID(t *testing.T) {
	lockFile := filepath.Join(t.TempDir(), "weft-agent.lock")
	if err := os.WriteFile(lockFile, []byte("999999"), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	if !shouldReclaimBuildLock(lockFile) {
		t.Fatal("lock with dead PID should be reclaimed")
	}
}

func TestShouldReclaimBuildLockReclaimsZombiePID(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcessExitImmediatelyForBuildLock")
	cmd.Env = append(os.Environ(), "WEFT_AGENTDEPLOY_HELPER=exit-immediately")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(2 * time.Second)
	for !processIsZombie(cmd.Process.Pid) {
		if time.Now().After(deadline) {
			t.Skip("could not observe helper process as zombie")
		}
		time.Sleep(10 * time.Millisecond)
	}

	lockFile := filepath.Join(t.TempDir(), "weft-agent.lock")
	if err := os.WriteFile(lockFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	if !shouldReclaimBuildLock(lockFile) {
		t.Fatal("lock with zombie PID should be reclaimed")
	}
}

func TestHelperProcessExitImmediatelyForBuildLock(t *testing.T) {
	if os.Getenv("WEFT_AGENTDEPLOY_HELPER") != "exit-immediately" {
		return
	}
	os.Exit(0)
}

func processIsZombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}
