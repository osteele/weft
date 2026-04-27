package terminal

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/logging"
)

// InstallTUIStdioCapture suppresses slog output and redirects fd 1/2 to a
// drain log so writes from any source cannot corrupt the TUI. It returns a
// tea.ProgramOption to route the program's own output back to the real
// terminal, plus a restore function to call after Run().
//
// On capture failure (e.g. on a platform without dup2), it falls back to
// logger suppression only. The TUI still works; uncaught writes can still
// corrupt the screen, but no worse than before.
func InstallTUIStdioCapture() (tea.ProgramOption, func()) {
	restoreLog := logging.Suppress()
	capture, err := logging.StartStdioCapture(logging.DefaultStdioCaptureLog())
	if err != nil {
		return func(*tea.Program) {}, restoreLog
	}
	opt := tea.WithOutput(capture.TTYStdout)
	restore := func() {
		capture.Restore()
		restoreLog()
	}
	return opt, restore
}
