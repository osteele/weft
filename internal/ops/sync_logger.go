package ops

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/ssh"
)

// connFilterHandler is a slog.Handler that drops log records containing
// SSH connection errors in the message or error attribute. All other
// records are forwarded to the wrapped base handler.
type connFilterHandler struct {
	base slog.Handler
}

func (h *connFilterHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.base.Enabled(ctx, level)
}

func (h *connFilterHandler) Handle(ctx context.Context, r slog.Record) error {
	if ssh.IsConnectionError(r.Message) {
		return nil
	}
	// Also check error/output attributes for SSH connection errors
	var drop bool
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "error" || a.Key == "output" {
			if ssh.IsConnectionError(fmt.Sprint(a.Value.Any())) {
				drop = true
				return false
			}
		}
		return true
	})
	if drop {
		return nil
	}
	return h.base.Handle(ctx, r)
}

func (h *connFilterHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &connFilterHandler{base: h.base.WithAttrs(attrs)}
}

func (h *connFilterHandler) WithGroup(name string) slog.Handler {
	return &connFilterHandler{base: h.base.WithGroup(name)}
}

// NewQuietSyncLogger returns a logger that suppresses SSH connection errors.
// Use this for background/implicit syncs (status, list, TUI) where offline
// hosts are expected and should not produce stderr noise.
func NewQuietSyncLogger() *slog.Logger {
	base := slog.NewTextHandler(os.Stderr, nil)
	return slog.New(&connFilterHandler{base: base})
}

// NewSilentSyncLogger returns a logger that discards all output.
func NewSilentSyncLogger() *slog.Logger {
	return logging.Discard()
}
