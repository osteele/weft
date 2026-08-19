package terminal

import (
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"

	"github.com/atotto/clipboard"
	tea "github.com/charmbracelet/bubbletea"
)

// clipboardMaxPayload bounds what a copy action will emit. OSC 52 has no
// reply, so an oversize payload is refused outright rather than truncated
// into a misleading clipboard.
const clipboardMaxPayload = 64 * 1024

// tuiTTYWriter holds the writable handle to the real terminal for the running
// TUI. fd 1 is dup2'd over a capture log for the program's lifetime (see
// internal/logging/stdio_capture.go), so writes to os.Stdout never reach the
// terminal — only the writer stored here does.
var tuiTTYWriter atomic.Value // ttyWriterBox, stored by InstallTUIStdioCapture

// clipboardWriteFunc performs the native clipboard write. It is a seam for
// tests, which must never touch a real clipboard.
var clipboardWriteFunc = clipboard.WriteAll

// ttyWriterBox keeps tuiTTYWriter's stored concrete type stable so tests can
// swap in a buffer without tripping atomic.Value's consistent-type check.
type ttyWriterBox struct{ w io.Writer }

// currentTTYWriter returns the real-terminal writer for the running TUI, or
// nil when no TUI has installed one.
func currentTTYWriter() io.Writer {
	if box, ok := tuiTTYWriter.Load().(ttyWriterBox); ok {
		return box.w
	}
	return nil
}

// clipboardCopiedMsg reports the outcome of a copy action. OSC 52 has no
// reply, so a terminal that ignored the sequence is indistinguishable from
// one that honored it; only a successful native write justifies "copied".
type clipboardCopiedMsg struct {
	label    string
	nativeOK bool // native clipboard write succeeded
	osc52    bool // OSC 52 sequence was emitted to the TTY
	remote   bool // session is remote, so no native write was attempted
	tooLarge bool
	size     int
}

// flashText renders the outcome as a flash message and whether it is an
// error. The label names the object; the verb encodes only what the evidence
// supports.
func (msg clipboardCopiedMsg) flashText() (string, bool) {
	switch {
	case msg.tooLarge:
		return fmt.Sprintf("too large to copy (%d bytes)", msg.size), true
	case msg.nativeOK:
		return msg.label + " copied to clipboard", false
	case msg.osc52 && msg.remote:
		return msg.label + " sent to terminal clipboard", false
	case msg.osc52:
		return msg.label + " sent to terminal clipboard (local clipboard unavailable)", false
	default:
		return "copy failed: no clipboard available", true
	}
}

// sessionIsRemote reports whether THIS process sits at the far end of an SSH
// session. It does not detect mosh, nested sessions, or a locally-running
// tmux attached from elsewhere. Because of that we emit OSC 52
// unconditionally and use this only to decide whether to ALSO attempt a
// native write, and how confidently the flash may phrase the result.
func sessionIsRemote() bool {
	return os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_CLIENT") != "" || os.Getenv("SSH_TTY") != ""
}

// copyToClipboardCmd copies payload under the given label. It runs as a
// tea.Cmd — never inside Update — because the native write forks
// pbcopy/xclip/wl-copy and must not stall the update loop. OSC 52 is always
// emitted to the real TTY (it is the only mechanism that reaches the human's
// clipboard over SSH); a native write is attempted additionally when the
// session is local.
func copyToClipboardCmd(label, payload string) tea.Cmd {
	return func() tea.Msg {
		if len(payload) > clipboardMaxPayload {
			return clipboardCopiedMsg{label: label, tooLarge: true, size: len(payload)}
		}
		msg := clipboardCopiedMsg{label: label, remote: sessionIsRemote()}
		if tty := currentTTYWriter(); tty != nil {
			if _, err := io.WriteString(tty, osc52Sequence(payload)); err == nil {
				msg.osc52 = true
			}
		}
		if !msg.remote {
			msg.nativeOK = clipboardWriteFunc(payload) == nil
		}
		return msg
	}
}

// osc52Sequence frames payload as an OSC 52 clipboard sequence, wrapped for
// multiplexers the way termenv (via go-osc52) does: tmux gets its passthrough
// form, screen a DCS wrapper with the base64 split into 76-byte chunks.
func osc52Sequence(payload string) string {
	b64 := base64.StdEncoding.EncodeToString([]byte(payload))
	switch {
	case os.Getenv("TMUX") != "":
		return "\x1bPtmux;\x1b\x1b]52;c;" + b64 + "\x07\x1b\\"
	case strings.HasPrefix(os.Getenv("TERM"), "screen"):
		chunks := make([]string, 0, len(b64)/76+1)
		for i := 0; i < len(b64); i += 76 {
			chunks = append(chunks, b64[i:min(i+76, len(b64))])
		}
		return "\x1bP\x1b]52;c;" + strings.Join(chunks, "\x1b\\\x1bP") + "\x07\x1b\\"
	default:
		return "\x1b]52;c;" + b64 + "\x07"
	}
}
