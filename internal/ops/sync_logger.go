package ops

import (
	"io"
	"log"
	"os"

	"github.com/osteele/weft/internal/ssh"
)

// connFilterWriter is an io.Writer that drops log lines containing SSH
// connection errors (timeouts, refused, unreachable, etc.). All other
// messages are forwarded to stderr.
type connFilterWriter struct{}

func (w *connFilterWriter) Write(p []byte) (int, error) {
	if ssh.IsConnectionError(string(p)) {
		return len(p), nil // silently discard
	}
	return os.Stderr.Write(p)
}

// NewQuietSyncLogger returns a logger that suppresses SSH connection errors.
// Use this for background/implicit syncs (status, list, TUI) where offline
// hosts are expected and should not produce stderr noise.
func NewQuietSyncLogger() *log.Logger {
	return log.New(&connFilterWriter{}, "", log.LstdFlags)
}

// NewSilentSyncLogger returns a logger that discards all output.
// Use this for background syncs in list/project commands where structured
// warnings from HostSyncResult are sufficient.
func NewSilentSyncLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}
