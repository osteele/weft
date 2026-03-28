package logging

import (
	"context"
	"io"
	"log/slog"
	"sync"
)

// LevelOff is a log level high enough to suppress all output.
const LevelOff = slog.Level(100)

var level slog.LevelVar

// Setup configures the default slog logger.
// mode "json" uses JSONHandler (for agent); anything else uses TextHandler.
func Setup(w io.Writer, mode string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: &level}
	var handler slog.Handler
	if mode == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

// SetLevel changes the effective log level.
func SetLevel(l slog.Level) {
	level.Set(l)
}

// GetLevel returns the current log level.
func GetLevel() slog.Level {
	return level.Level()
}

// Discard returns a logger that discards all output.
func Discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Suppress sets the log level to LevelOff, suppressing all output.
// It returns a function that restores the previous level.
func Suppress() (restore func()) {
	prev := level.Level()
	level.Set(LevelOff)
	return func() {
		level.Set(prev)
	}
}

// CapturingHandler collects log messages at or above a threshold level.
// It is concurrency-safe.
type CapturingHandler struct {
	threshold slog.Level
	mu        sync.Mutex
	messages  []string
}

// NewCapturingHandler creates a handler that captures messages at or above
// the given threshold. All messages are captured (none are forwarded).
func NewCapturingHandler(threshold slog.Level) *CapturingHandler {
	return &CapturingHandler{threshold: threshold}
}

func (h *CapturingHandler) Enabled(_ context.Context, l slog.Level) bool {
	return l >= h.threshold
}

func (h *CapturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.messages = append(h.messages, r.Message)
	h.mu.Unlock()
	return nil
}

func (h *CapturingHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

func (h *CapturingHandler) WithGroup(string) slog.Handler {
	return h
}

// Messages returns the captured messages.
func (h *CapturingHandler) Messages() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	result := make([]string, len(h.messages))
	copy(result, h.messages)
	return result
}
