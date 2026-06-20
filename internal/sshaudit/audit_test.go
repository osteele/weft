package sshaudit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/osteele/weft/internal/oplog"
)

func TestSplitTarget(t *testing.T) {
	tests := []struct {
		target string
		user   string
		host   string
	}{
		{target: "cool30", host: "cool30"},
		{target: "agent@studio", user: "agent", host: "studio"},
		{target: "root@203.0.113.10:/tmp/agent", user: "root", host: "203.0.113.10"},
		{target: "ssh://root@example.test/tmp", user: "root", host: "example.test"},
		{target: "[2001:db8::1]:22", host: "2001:db8::1"},
		{target: "root@[2001:db8::1]:22", user: "root", host: "2001:db8::1"},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			gotUser, gotHost := SplitTarget(tt.target)
			if gotUser != tt.user || gotHost != tt.host {
				t.Fatalf("SplitTarget(%q) = (%q, %q), want (%q, %q)", tt.target, gotUser, gotHost, tt.user, tt.host)
			}
		})
	}
}

func TestLogWritesOpsAndAuditFile(t *testing.T) {
	dir := t.TempDir()
	opsPath := filepath.Join(dir, "operations.log")
	auditPath := filepath.Join(dir, "ssh-audit.jsonl")
	t.Setenv("WEFT_SSH_AUDIT_LOG", auditPath)

	if err := oplog.Init(opsPath, 0); err != nil {
		t.Fatalf("oplog.Init: %v", err)
	}
	t.Cleanup(func() { _ = oplog.Close() })

	Log(KindCommand, "agent@studio", 0)
	if err := oplog.Close(); err != nil {
		t.Fatalf("oplog.Close: %v", err)
	}

	opsEntries, err := oplog.ReadEntries(opsPath)
	if err != nil {
		t.Fatalf("oplog.ReadEntries: %v", err)
	}
	if len(opsEntries) != 1 {
		t.Fatalf("ops entries = %d, want 1", len(opsEntries))
	}
	if opsEntries[0].Operation != oplog.OpSSH || opsEntries[0].Host != "studio" {
		t.Fatalf("ops entry = %+v, want ssh.exec on studio", opsEntries[0])
	}
	if opsEntries[0].Detail == "" || !containsAll(opsEntries[0].Detail, "user=agent", "host=studio", "kind=command") {
		t.Fatalf("ops detail = %q, want user/host/kind", opsEntries[0].Detail)
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	var entry fileEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("unmarshal audit entry: %v", err)
	}
	if entry.Kind != KindCommand || entry.User != "agent" || entry.Host != "studio" || entry.Target != "agent@studio" {
		t.Fatalf("audit entry = %+v, want command agent@studio", entry)
	}
}

func containsAll(s string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(s, needle) {
			return false
		}
	}
	return true
}
