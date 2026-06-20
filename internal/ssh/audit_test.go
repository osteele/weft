package ssh

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunWithTimeoutAuditsMockedSSH(t *testing.T) {
	dir := t.TempDir()
	auditPath := filepath.Join(dir, "ssh-audit.jsonl")
	t.Setenv("WEFT_SSH_AUDIT_LOG", auditPath)

	cleanup := SetRunner(func(host, command string) (string, string, error) {
		return "ok\n", "", nil
	})
	t.Cleanup(cleanup)

	if _, _, err := RunWithTimeout("agent@studio", "true", 5*time.Second); err != nil {
		t.Fatalf("RunWithTimeout: %v", err)
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	line := string(data)
	for _, want := range []string{`"kind":"command"`, `"user":"agent"`, `"host":"studio"`, `"timeout_ms":5000`} {
		if !strings.Contains(line, want) {
			t.Fatalf("audit log = %q, want %s", line, want)
		}
	}
}
