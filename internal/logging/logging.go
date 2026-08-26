package logging

import (
	"context"
	"io"
	"log/slog"
	"sync"

	"github.com/osteele/weft/internal/secrets"
)

// LevelOff is a log level high enough to suppress all output.
const LevelOff = slog.Level(100)

var level slog.LevelVar

// Setup configures the default slog logger.
// mode "json" uses JSONHandler (for agent); anything else uses TextHandler.
func Setup(w io.Writer, mode string) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       &level,
		ReplaceAttr: redactAttr,
	}
	var handler slog.Handler
	if mode == "json" {
		handler = slog.NewJSONHandler(w, opts)
	} else {
		handler = slog.NewTextHandler(w, opts)
	}
	handler = redactingHandler{next: handler}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

type redactingHandler struct {
	next slog.Handler
}

func (h redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	redacted := slog.NewRecord(record.Time, record.Level, secrets.RedactText(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		redacted.AddAttrs(attr)
		return true
	})
	return h.next.Handle(ctx, redacted)
}

func (h redactingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return redactingHandler{next: h.next.WithAttrs(attrs)}
}

func (h redactingHandler) WithGroup(name string) slog.Handler {
	return redactingHandler{next: h.next.WithGroup(name)}
}

func redactAttr(_ []string, attr slog.Attr) slog.Attr {
	switch attr.Value.Kind() {
	case slog.KindString:
		attr.Value = slog.StringValue(secrets.RedactText(attr.Value.String()))
	case slog.KindAny:
		if err, ok := attr.Value.Any().(error); ok {
			attr.Value = slog.StringValue(secrets.RedactText(err.Error()))
		}
	}
	return attr
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
	h.messages = append(h.messages, secrets.RedactText(r.Message))
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
