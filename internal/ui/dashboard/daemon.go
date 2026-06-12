package dashboard

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/daemoncontrol"
)

func (m *Model) ensureCurrentDaemonForTick() tea.Cmd {
	if m.daemonRestartInProgress {
		return nil
	}
	status, err := daemoncontrol.CurrentStatus(daemoncontrol.DefaultPaths())
	if err != nil || !status.ActiveBinaryStale {
		return nil
	}
	m.daemonRestartInProgress = true
	return func() tea.Msg {
		status, action, err := daemoncontrol.EnsureCurrent(daemoncontrol.DefaultPaths(), 2*time.Second)
		return daemonRestartedMsg{pid: status.PID, action: action, err: err}
	}
}
