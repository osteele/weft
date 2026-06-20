package sshaudit

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/oplog"
)

const (
	KindCommand     = "command"
	KindInteractive = "interactive"
	KindStreaming   = "streaming"
	KindSCP         = "scp"
)

var auditMu sync.Mutex

type fileEntry struct {
	Time      time.Time `json:"t"`
	Kind      string    `json:"kind"`
	User      string    `json:"user,omitempty"`
	Host      string    `json:"host"`
	Target    string    `json:"target"`
	TimeoutMS int64     `json:"timeout_ms,omitempty"`
	PID       int       `json:"pid"`
}

// Log records that weft is about to invoke ssh/scp for the target. It records
// the resolved username and host when they are explicit; otherwise the user is
// the local account that ssh will use by default unless ~/.ssh/config overrides
// it.
func Log(kind, target string, timeout time.Duration) {
	userName, host := SplitTarget(target)
	if userName == "" {
		userName = localUser()
	}
	detail := "kind=" + kind + " user=" + userName + " host=" + host
	if target != host && target != "" {
		detail += " target=" + target
	}
	if timeout > 0 {
		detail += " timeout_ms=" + strconv.FormatInt(timeout.Milliseconds(), 10)
	}
	oplog.Log(oplog.OpSSH, oplog.WithHost(host), oplog.WithDetail(detail))
	writeFileEntry(fileEntry{
		Time:      time.Now(),
		Kind:      kind,
		User:      userName,
		Host:      host,
		Target:    target,
		TimeoutMS: timeout.Milliseconds(),
		PID:       os.Getpid(),
	})
}

// SplitTarget returns the user and host from ssh targets like "user@host",
// "host", "ssh://user@host", or scp-ish "user@host:path".
func SplitTarget(target string) (string, string) {
	t := strings.TrimSpace(target)
	if t == "" {
		return "", ""
	}
	if rest, ok := strings.CutPrefix(t, "ssh://"); ok {
		t = rest
	}
	if idx := strings.Index(t, "/"); idx >= 0 {
		t = t[:idx]
	}
	userName := ""
	if user, host, ok := strings.Cut(t, "@"); ok {
		userName = user
		t = host
	}
	if strings.HasPrefix(t, "[") {
		if idx := strings.Index(t, "]"); idx >= 0 {
			return userName, t[1:idx]
		}
	}
	if idx := strings.Index(t, ":"); idx >= 0 {
		t = t[:idx]
	}
	return userName, t
}

func writeFileEntry(entry fileEntry) {
	path := auditLogPath()
	if path == "" {
		return
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_ = json.NewEncoder(f).Encode(entry)
}

func auditLogPath() string {
	if path := strings.TrimSpace(os.Getenv("WEFT_SSH_AUDIT_LOG")); path != "" {
		return path
	}
	if !isTestBinary() {
		return ""
	}
	if path := strings.TrimSpace(os.Getenv("WEFT_TEST_SSH_LOG")); path != "" {
		return path
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return filepath.Join(u.HomeDir, ".cache", "weft", "test-ssh.jsonl")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".cache", "weft", "test-ssh.jsonl")
	}
	return filepath.Join(os.TempDir(), "weft-test-ssh.jsonl")
}

func isTestBinary() bool {
	return strings.HasSuffix(filepath.Base(os.Args[0]), ".test")
}

func localUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		if _, name, ok := strings.Cut(u.Username, `\`); ok {
			return name
		}
		return u.Username
	}
	return strings.TrimSpace(os.Getenv("USER"))
}
