package agentdeploy

import (
	"os"
	"path/filepath"
	"strconv"
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
