package dashboard

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/monitor"
	"github.com/osteele/weft/internal/progress"
	"github.com/osteele/weft/internal/queueblock"
)

func (m Model) handleJobsRefreshed(msg jobsRefreshedMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.setFlash(fmt.Sprintf("Error loading jobs: %v", msg.err), true)
	}
	m.allJobs = msg.jobs
	queueblock.Apply(m.allJobs, queueblock.FromHosts(m.hosts))
	if msg.jobDependencies != nil {
		m.jobDependencies = msg.jobDependencies
	}
	m.applyJobFilter()

	runningJobIDs := make(map[int64]bool)
	for _, job := range msg.jobs {
		if job.EffectiveStatus() == db.StatusRunning {
			runningJobIDs[job.ID] = true
		}
	}
	for jobID := range m.jobProgress {
		if !runningJobIDs[jobID] {
			delete(m.jobProgress, jobID)
		}
	}

	if m.pendingSelectJobID > 0 {
		for i, job := range m.jobs {
			if job.ID == m.pendingSelectJobID {
				m.jobList.Select(i)
				break
			}
		}
		m.pendingSelectJobID = 0
	}

	var cmds []tea.Cmd

	// Trigger initial priority sync now that jobs are loaded
	if m.initialSyncNeeded && m.syncWorker != nil {
		m.initialSyncNeeded = false
		m.requestSyncAllHosts(true)
	}

	if cmd := m.fetchAllRunningJobsProgress(); cmd != nil {
		cmds = append(cmds, cmd)
	}
	if m.showHostSummaries && m.llmGenerator != nil {
		cmds = append(cmds, m.generateAllHostSummaries())
	}

	if len(cmds) > 0 {
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m Model) handleHostsLoaded(msg hostsLoadedMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.setFlash(fmt.Sprintf("Error loading hosts: %v", msg.err), true)
	}
	monitorHosts := map[string]*Host{}
	if m.monitor != nil {
		for _, host := range m.monitor.Hosts() {
			if host != nil && host.Name != "" {
				monitorHosts[host.Name] = host
			}
		}
	}
	var cmds []tea.Cmd
	for _, name := range msg.hostNames {
		found := false
		for _, h := range m.hosts {
			if h.Name == name {
				found = true
				break
			}
		}
		if !found {
			var host *Host
			if monitorHost, ok := monitorHosts[name]; ok {
				host = monitorHost
				m.hosts = append(m.hosts, host)
				continue
			}
			cachedInfo, err := db.LoadCachedHostInfo(m.database, name)
			if err == nil && cachedInfo != nil {
				host = hostFromCachedInfo(cachedInfo)
				cacheAge := time.Since(time.Unix(cachedInfo.LastUpdated, 0))
				if cacheAge > m.hostCacheDuration && m.monitor == nil {
					host.Status = HostStatusChecking
					m.requestHostInfoRefresh(name, false)
				}
			} else {
				host = &Host{
					Name:   name,
					Status: HostStatusChecking,
				}
				if m.monitor == nil {
					m.requestHostInfoRefresh(name, false)
				}
			}
			m.hosts = append(m.hosts, host)
		}
		if !m.hostsQueriedThisSession[name] {
			m.requestHostInfoRefresh(name, false)
		}
	}
	if len(cmds) > 0 {
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

func (m Model) handleHostSyncTimesLoaded(msg hostSyncTimesLoadedMsg) (Model, tea.Cmd) {
	if msg.err != nil {
		return m, m.setFlash(fmt.Sprintf("Error loading host sync times: %v", msg.err), true)
	}
	if msg.times != nil {
		m.hostSyncTimes = msg.times
	}
	m.applyJobFilter()
	return m, nil
}

func (m Model) handleHostInfo(msg hostInfoMsg) (Model, tea.Cmd) {
	var cmds []tea.Cmd
	for i, h := range m.hosts {
		if h.Name == msg.hostName {
			// Hysteresis: require 3 consecutive failures before marking offline.
			// This prevents a single SSH timeout from wiping host metrics.
			if msg.info.Status == HostStatusOffline && h.Status == HostStatusOnline {
				m.hostFailCount[msg.hostName]++
				if m.hostFailCount[msg.hostName] < 3 {
					// Not enough failures yet; keep existing metrics but mark
					// status as checking so stale/degraded state is visible.
					m.hosts[i].Status = HostStatusChecking
					break
				}
			} else if msg.info.Status == HostStatusOnline {
				m.hostFailCount[msg.hostName] = 0
			}
			m.hosts[i].UpdateFrom(msg.info)

			// Warn if host has low disk space (only on first detection this session)
			if msg.info.HasLowDiskSpace() && !m.lowDiskWarnedHosts[msg.hostName] {
				m.lowDiskWarnedHosts[msg.hostName] = true
				cmds = append(cmds, m.setFlash(
					fmt.Sprintf("Warning: %s has low disk space (%s free)", msg.hostName, msg.info.DiskFreeSummary()),
					true,
				))
			}
			break
		}
	}
	m.hostsQueriedThisSession[msg.hostName] = true
	queueblock.Apply(m.allJobs, queueblock.FromHosts(m.hosts))
	if len(cmds) == 0 {
		return m, nil
	}
	return m, tea.Batch(cmds...)
}

func (m Model) handleSyncCompleted(msg syncCompletedMsg) (Model, tea.Cmd) {
	// Legacy handler - update host sync times
	now := time.Now()
	if len(msg.hostsSynced) > 0 {
		for _, host := range msg.hostsSynced {
			m.lastHostSyncTimes[host] = now
			m.hostSyncTimes[host] = now
		}
	}
	if msg.err != nil {
		return m, m.setFlash(fmt.Sprintf("Sync error: %v", msg.err), true)
	}
	// Only show flash messages for actual errors during periodic sync.
	// Don't flash "Synced N jobs" or "Started queue" - those are normal operations.
	var cmds []tea.Cmd
	if len(msg.queueRunnerErrors) > 0 {
		label := "error"
		if len(msg.queueRunnerErrors) > 1 {
			label = "errors"
		}
		cmds = append(cmds, m.setFlash(fmt.Sprintf("Queue runner %s: %s", label, strings.Join(msg.queueRunnerErrors, "; ")), true))
	}
	if m.monitor == nil {
		cmds = append(cmds, m.refreshJobs(), m.loadHosts())
	}
	if len(cmds) == 0 {
		return m, nil
	}
	return m, tea.Batch(cmds...)
}

func (m Model) handleMonitorEvent(msg monitorEventMsg) (Model, tea.Cmd) {
	event := msg.event
	var cmd tea.Cmd
	switch event.Type {
	case monitor.EventJobsRefreshed:
		m, cmd = m.handleJobsRefreshed(jobsRefreshedMsg{
			jobs:            event.Jobs,
			jobDependencies: event.JobDependencies,
			err:             event.Err,
		})
	case monitor.EventHostsLoaded:
		m, cmd = m.handleHostsLoaded(hostsLoadedMsg{
			hostNames: event.HostNames,
			err:       event.Err,
		})
	case monitor.EventHostInfoUpdated:
		if event.Host != nil {
			m, cmd = m.handleHostInfo(hostInfoMsg{
				hostName: event.Host.Name,
				info:     event.Host,
			})
		}
	case monitor.EventHostSyncTimesLoaded:
		m, cmd = m.handleHostSyncTimesLoaded(hostSyncTimesLoadedMsg{
			times: event.HostSyncTimes,
			err:   event.Err,
		})
	case monitor.EventSyncCompleted:
		m, cmd = m.handleSyncCompleted(syncCompletedMsg{
			updated:           event.SyncResult.Updated,
			queuesStarted:     event.SyncResult.QueuesStarted,
			hostsSynced:       event.SyncResult.HostsSynced,
			queueRunnerErrors: event.SyncResult.QueueRunnerErrors,
			err:               event.Err,
		})
	case monitor.EventJobLogFetched:
		if event.JobLog != nil {
			m, cmd = m.handleMonitorJobLog(event.JobLog)
		}
	case monitor.EventProcessStatsFetched:
		if event.ProcessStats != nil {
			m, cmd = m.handleMonitorProcessStats(event.ProcessStats)
		}
	case monitor.EventError:
		if event.Err != nil {
			cmd = m.setFlash(fmt.Sprintf("Monitor error: %v", event.Err), true)
		}
	}
	waitCmd := m.waitForMonitorEvent()
	if cmd == nil {
		return m, waitCmd
	}
	if waitCmd == nil {
		return m, cmd
	}
	return m, tea.Batch(cmd, waitCmd)
}

func (m Model) handleMonitorJobLog(result *monitor.JobLogResult) (Model, tea.Cmd) {
	m.logLoading = false

	// Extract progress info
	if result.Err == nil {
		prog := progress.FindLastProgressPreferExplicit(result.Content)
		if prog != nil {
			m.jobProgress[result.JobID] = prog
		}
	}

	if m.selectedJob == nil || result.JobID != m.selectedJob.ID {
		return m, nil
	}

	if result.Err != nil {
		if result.ConnError {
			// Try local cache on connection error
			if cached, err := logcache.Read(result.JobID); err == nil {
				m.logContent = cached
				m.logStale = true
				m.logCache[result.JobID] = cached
			} else if cached, ok := m.logCache[result.JobID]; ok {
				m.logContent = cached
				m.logStale = true
			} else {
				m.logContent = fmt.Sprintf("Host %s unreachable", m.selectedJob.Host)
				m.logStale = false
			}
		} else {
			combined := result.Content + result.Stderr
			if strings.Contains(combined, "No such file") || strings.Contains(combined, "cannot open") {
				msg := "No log file yet"
				if db.IsTerminalStatus(m.selectedJob.EffectiveStatus()) {
					msg = "Log file not found (may have been cleaned up)"
				}
				m.logContent = msg
			} else {
				m.logContent = fmt.Sprintf("Error: %s", strings.TrimSpace(combined))
			}
			m.logStale = false
		}
	} else {
		combined := result.Content
		if strings.Contains(combined, "No such file") || strings.Contains(combined, "cannot open") {
			msg := "No log file yet"
			if db.IsTerminalStatus(m.selectedJob.EffectiveStatus()) {
				msg = "Log file not found (may have been cleaned up)"
			}
			m.logContent = msg
			m.logStale = false
		} else {
			m.logCache[result.JobID] = result.Content
			m.logContent = result.Content
			m.logStale = false
		}
	}

	m.logViewport.SetContent(m.logContent)
	m.logViewport.GotoBottom()
	return m, nil
}

func (m Model) handleMonitorProcessStats(result *monitor.ProcessStatsResult) (Model, tea.Cmd) {
	targetJob := m.getTargetJob()
	if targetJob == nil || result.JobID != targetJob.ID || result.Stats == nil {
		return m, nil
	}

	stats := result.Stats
	if stats.Running || m.processStats == nil || m.processStatsJobID != result.JobID {
		if m.prevProcessStats != nil && m.processStatsJobID == result.JobID &&
			stats.Timestamp > m.prevProcessStats.Timestamp && stats.Running {
			deltaTicks := (stats.CPUUserTicks + stats.CPUSysTicks) -
				(m.prevProcessStats.CPUUserTicks + m.prevProcessStats.CPUSysTicks)
			deltaTime := stats.Timestamp - m.prevProcessStats.Timestamp
			if deltaTime > 0 {
				stats.CPUPct = float64(deltaTicks) / float64(deltaTime)
			}
		}
		if stats.Running {
			m.prevProcessStats = m.processStats
		}
		m.processStats = stats
		m.processStatsJobID = result.JobID
	}

	return m, nil
}

// handleSelectionChanged is called when the job list selection changes.
// It clears cached process stats and fetches logs/stats for the new selection.
func (m *Model) handleSelectionChanged() tea.Cmd {
	m.jobSelectionActive = true
	// Clear cached process stats when changing jobs
	m.processStats = nil
	m.prevProcessStats = nil
	m.processStatsJobID = 0

	idx := m.jobList.Index()
	if len(m.jobs) == 0 || idx < 0 || idx >= len(m.jobs) {
		return nil
	}

	job := m.jobs[idx]

	if m.detailTab == DetailTabDetails {
		m.detailViewport.GotoTop()
	}

	var cmds []tea.Cmd
	// If in Logs tab, fetch logs for new job
	if m.detailTab == DetailTabLogs {
		m.selectedJob = job
		m.logLoading = true
		// Show cached content immediately while fetching fresh logs
		if cached, ok := m.logCache[job.ID]; ok {
			m.logContent = cached
			m.logStale = true
			m.logViewport.SetContent(m.logContent)
		} else {
			m.logContent = ""
			m.logStale = false
		}
		if m.monitor != nil {
			m.monitor.WatchJobLog(job)
			if job.EffectiveStatus() == db.StatusRunning {
				m.monitor.WatchJobStats(job)
			}
		} else {
			cmds = append(cmds, m.fetchSelectedJobLog())
		}
		if len(cmds) > 0 {
			return tea.Batch(cmds...)
		}
		return nil
	}

	// Fetch CPU summary if that tab is active
	if m.detailTab == DetailTabCPU {
		if cmd := m.requestJobCPUTop(job); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	// Pre-fetch log for non-Logs tab (background cache warming)
	if m.monitor == nil {
		if _, ok := m.logCache[job.ID]; !ok || job.EffectiveStatus() == db.StatusRunning {
			if cmd := m.fetchJobLog(job); cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}

	// Update watched stats job for running jobs
	if m.monitor != nil {
		if job.EffectiveStatus() == db.StatusRunning {
			m.monitor.WatchJobStats(job)
		} else {
			m.monitor.WatchJobStats(nil)
		}
	}

	if len(cmds) > 0 {
		return tea.Batch(cmds...)
	}
	return nil
}

func (m *Model) handleHostSelectionChanged() tea.Cmd {
	if len(m.hosts) == 0 || m.selectedHostIdx < 0 || m.selectedHostIdx >= len(m.hosts) {
		if m.hostDetailTab == HostDetailTabCPU {
			return m.requestHostCPUTop("")
		}
		return nil
	}
	host := m.hosts[m.selectedHostIdx]
	if m.hostDetailTab == HostDetailTabCPU {
		return m.requestHostCPUTop(host.Name)
	}
	return nil
}
