package dashboard

import (
	"database/sql"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/app/hostsync"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/queueblock"
	"github.com/osteele/weft/internal/r2"
)

type SyncRate = hostsync.SyncRate

const (
	RateIdle    = hostsync.RateIdle
	RateQueued  = hostsync.RateQueued
	RateRunning = hostsync.RateRunning
	RateWarmup  = hostsync.RateWarmup
)

type SyncRequest = hostsync.Request
type SyncResult = hostsync.Result
type SyncWorker = hostsync.Worker

func NewSyncWorker(database *sql.DB, cloudClients []cloud.Client, r2Client *r2.Client, appConfig *config.Config) *SyncWorker {
	return hostsync.New(database, cloudClients, r2Client, appConfig)
}

func GetHostSyncRate(jobs []*db.Job) SyncRate {
	return hostsync.GetHostSyncRate(jobs)
}

func GetHostSyncMode(jobs []*db.Job) ops.SyncMode {
	return hostsync.GetHostSyncMode(jobs)
}

func buildSyncWarning(result ops.HostSyncResult) string {
	return hostsync.BuildWarning(result)
}

// syncResultMsg wraps a SyncResult for the TUI message loop.
type syncResultMsg struct {
	result SyncResult
}

// requestSyncsForActiveHosts sends sync requests for all hosts with jobs.
func (m *Model) requestSyncsForActiveHosts() {
	if m.syncWorker == nil {
		return
	}

	jobsByHost := make(map[string][]*db.Job)
	for _, job := range m.allJobs {
		if job != nil {
			jobsByHost[job.Host] = append(jobsByHost[job.Host], job)
		}
	}

	for host, jobs := range jobsByHost {
		rate := GetHostSyncRate(jobs)
		m.syncWorker.Request(SyncRequest{
			Host: host,
			Rate: rate,
			Mode: GetHostSyncMode(jobs),
		})
	}

	for _, host := range m.hosts {
		if _, hasJobs := jobsByHost[host.Name]; !hasJobs {
			m.syncWorker.Request(SyncRequest{
				Host: host.Name,
				Rate: RateIdle,
				Mode: ops.SyncModeStatus,
			})
		}
	}
}

// requestSyncAllHosts requests sync for all known hosts.
func (m *Model) requestSyncAllHosts(priority bool) {
	if m.syncWorker == nil {
		return
	}

	hosts := make(map[string]bool)
	for _, job := range m.allJobs {
		if job != nil {
			hosts[job.Host] = true
		}
	}
	for _, host := range m.hosts {
		hosts[host.Name] = true
	}

	for host := range hosts {
		mode := ops.SyncModeStatus
		if priority {
			mode = ops.SyncModeFull
		}
		m.syncWorker.Request(SyncRequest{
			Host:     host,
			Rate:     RateRunning,
			Priority: priority,
			Mode:     mode,
		})
	}
}

// checkSyncResults returns a command that checks for sync results.
func (m *Model) checkSyncResults() tea.Cmd {
	if m.syncWorker == nil {
		return nil
	}
	return func() tea.Msg {
		select {
		case result, ok := <-m.syncWorker.Results():
			if ok {
				return syncResultMsg{result: result}
			}
		default:
		}
		return nil
	}
}

func (m *Model) waitForSyncResult() tea.Cmd {
	if m.syncWorker == nil {
		return nil
	}
	return m.syncWorker.WaitForResult(m.ctx, func(result SyncResult) tea.Msg {
		return syncResultMsg{result: result}
	})
}

// handleSyncResult handles a sync result from the worker.
func (m Model) handleSyncResult(msg syncResultMsg) (Model, tea.Cmd) {
	result := msg.result

	now := time.Now()
	m.lastHostSyncTimes[result.Host] = now
	m.hostSyncTimes[result.Host] = now

	if result.Error != nil {
		m.hostFailCount[result.Host]++
		if m.hostFailCount[result.Host] >= 3 {
			for i, h := range m.hosts {
				if h.Name == result.Host {
					m.hosts[i].Status = HostStatusOffline
					m.hosts[i].Error = result.Error.Error()
					break
				}
			}
		}
		return m, m.setFlash(fmt.Sprintf("Sync error (%s): %v", result.Host, result.Error), true)
	}
	m.hostFailCount[result.Host] = 0

	var cmds []tea.Cmd

	hostRef := func() *Host {
		for _, host := range m.hosts {
			if host.Name == result.Host {
				return host
			}
		}
		return nil
	}
	if result.HostFull != nil || result.HostInfo != nil {
		var host *Host
		if result.HostFull != nil {
			host = result.HostFull
			host.Status = HostStatusOnline
		} else {
			host = hostFromCachedInfo(result.HostInfo)
		}
		m.hostsQueriedThisSession[result.Host] = true
		found := false
		for i, h := range m.hosts {
			if h.Name == result.Host {
				m.hosts[i].UpdateFrom(host)
				found = true
				break
			}
		}
		if !found {
			m.hosts = append(m.hosts, host)
		}
	}
	if host := hostRef(); host != nil {
		host.SyncWarning = result.HostWarning
	} else if result.HostWarning != "" {
		m.hosts = append(m.hosts, &Host{
			Name:        result.Host,
			SyncWarning: result.HostWarning,
		})
	}

	if result.QueueStatus != nil {
		for i, h := range m.hosts {
			if h.Name == result.Host {
				m.hosts[i].QueueStatus = QueueCheckChecked
				m.hosts[i].QueueRunnerActive = result.QueueStatus.RunnerActive
				m.hosts[i].QueuedJobCount = result.QueueStatus.QueuedJobCount
				m.hosts[i].CurrentQueueJob = result.QueueStatus.CurrentJob
				m.hosts[i].QueueStopPending = result.QueueStatus.StopPending
				m.hosts[i].BlockedQueueJobs = result.QueueStatus.BlockedReasons

				if result.HostWarning == "" && !result.QueueStatus.RunnerActive && !m.queueStoppedWarnedHosts[result.Host] {
					queuedCount, _ := db.CountQueuedByHost(m.database, result.Host)
					if queuedCount > 0 {
						m.queueStoppedWarnedHosts[result.Host] = true
						cmds = append(cmds, m.setFlash(
							fmt.Sprintf("Warning: Queue runner on %s is not running but %d job(s) are waiting. Press 'S' to start it.", result.Host, queuedCount),
							true,
						))
					}
				}
				break
			}
		}
	}

	if result.HostWarning != "" {
		cmds = append(cmds, m.setFlash(fmt.Sprintf("Sync warning (%s): %s", result.Host, result.HostWarning), true))
	}
	queueblock.Apply(m.allJobs, queueblock.FromHosts(m.hosts))

	if result.Updated > 0 {
		cmds = append(cmds, m.refreshJobs())
	}

	cmds = append(cmds, m.waitForSyncResult())

	if len(cmds) == 0 {
		return m, nil
	}
	return m, tea.Batch(cmds...)
}
