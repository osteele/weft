package dashboard

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fsnotify/fsnotify"
	"github.com/osteele/weft/internal/app/dbwatch"
)

func (m Model) startSyncTicker() tea.Cmd {
	return tea.Tick(throttledInterval(m.syncActiveInterval, m.focused), func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func (m Model) startLogTicker() tea.Cmd {
	return tea.Tick(throttledInterval(m.logRefreshInterval, m.focused), func(t time.Time) tea.Msg {
		return logTickMsg(t)
	})
}

func (m Model) startCreateTicker() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return createTickMsg(t)
	})
}

func (m Model) startHostRefreshTicker() tea.Cmd {
	return tea.Tick(throttledInterval(m.hostRefreshInterval, m.focused), func(t time.Time) tea.Msg {
		return hostRefreshTickMsg(t)
	})
}

func (m Model) startHostSummaryTicker() tea.Cmd {
	return tea.Tick(throttledInterval(hostSummaryTickerInterval, m.focused), func(t time.Time) tea.Msg {
		return hostSummaryTickMsg(t)
	})
}

func (m Model) startDBWatcher() tea.Cmd {
	return func() tea.Msg {
		watcher, targets, err := dbwatch.OpenJobsDBWatcher()
		if err != nil {
			return dbWatcherReadyMsg{err: err}
		}
		if watcher == nil {
			return nil
		}
		return dbWatcherReadyMsg{
			watcher: watcher,
			targets: targets,
		}
	}
}

func (m Model) waitForDBEvent() tea.Cmd {
	if m.dbWatcher == nil || len(m.dbWatcherTargets) == 0 {
		return nil
	}
	watcher := m.dbWatcher
	targets := m.dbWatcherTargets

	return func() tea.Msg {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return dbWatchEventMsg{err: fmt.Errorf("db watcher closed")}
				}
				if !dbwatch.IsWatchedFile(event.Name, targets) {
					continue
				}
				if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
					continue
				}
				return dbWatchEventMsg{}
			case err, ok := <-watcher.Errors:
				if !ok {
					return dbWatchEventMsg{err: fmt.Errorf("db watcher error channel closed")}
				}
				return dbWatchEventMsg{err: err}
			}
		}
	}
}

func (m Model) waitForMonitorEvent() tea.Cmd {
	if m.monitor == nil || m.monitorEvents == nil {
		return nil
	}
	return func() tea.Msg {
		event, ok := <-m.monitorEvents
		if !ok {
			return nil
		}
		return monitorEventMsg{event: event}
	}
}

func (m Model) startMonitor() tea.Cmd {
	if m.monitor == nil {
		return nil
	}
	return func() tea.Msg {
		m.monitor.Start()
		return nil
	}
}

func (m Model) startCloudDiscovery() tea.Cmd {
	if m.cloudDiscoveryFn == nil {
		return nil
	}
	return func() tea.Msg {
		discovery := m.cloudDiscoveryFn(m.appConfig)
		return cloudDiscoveryLoadedMsg{
			clients: discovery.Clients,
			err:     discovery.UnavailableError(),
		}
	}
}

func (m Model) startLLMInit() tea.Cmd {
	if m.llmInitFn == nil {
		return nil
	}
	return func() tea.Msg {
		return llmGeneratorLoadedMsg{
			generator: m.llmInitFn(m.database, m.appConfig),
		}
	}
}

func isWatchedDBFile(name string, targets map[string]struct{}) bool {
	return dbwatch.IsWatchedFile(name, targets)
}
