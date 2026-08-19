package terminal

import (
	"io"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/logging"
)

// TUIStdio is the result of installing stdio capture for a TUI run.
type TUIStdio struct {
	// Option routes the tea program's own output to the real terminal.
	Option tea.ProgramOption
	// TTY writes to the real terminal even while fd 1/2 are redirected to the
	// capture log. When capture failed, stdout was never redirected, so TTY is
	// os.Stdout there and terminal writes (e.g. OSC 52) still work.
	TTY io.Writer
	// Restore undoes the capture; call it after Run().
	Restore func()
}

// InstallTUIStdioCapture suppresses slog output and redirects fd 1/2 to a
// drain log so writes from any source cannot corrupt the TUI. The returned
// Option routes the program's own output back to the real terminal; call
// Restore after Run().
//
// On capture failure (e.g. on a platform without dup2), it falls back to
// logger suppression only. The TUI still works; uncaught writes can still
// corrupt the screen, but no worse than before.
func InstallTUIStdioCapture() TUIStdio {
	restoreLog := logging.Suppress()
	capture, err := logging.StartStdioCapture(logging.DefaultStdioCaptureLog())
	if err != nil {
		tuiTTYWriter.Store(ttyWriterBox{w: os.Stdout})
		return TUIStdio{Option: func(*tea.Program) {}, TTY: os.Stdout, Restore: restoreLog}
	}
	tuiTTYWriter.Store(ttyWriterBox{w: capture.TTYStdout})
	restore := func() {
		capture.Restore()
		restoreLog()
	}
	return TUIStdio{Option: tea.WithOutput(capture.TTYStdout), TTY: capture.TTYStdout, Restore: restore}
}
