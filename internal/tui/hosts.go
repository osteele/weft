package tui

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/agentdeploy"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/queuerunner"
	"github.com/osteele/weft/internal/slack"
	"github.com/osteele/weft/internal/ssh"
)

var findHostSpecFunc = inventory.FindHost
var ensureAgentUpToDateFunc = agentdeploy.EnsureAgentUpToDate

func (m Model) loadHosts() tea.Cmd {
	database := m.database
	return func() tea.Msg {
		// Get hosts from jobs
		jobHosts, err := db.ListUniqueHosts(database)
		if err != nil {
			return hostsLoadedMsg{err: err}
		}

		// Get hosts from cache
		cachedHosts, err := db.LoadAllCachedHosts(database)
		if err != nil {
			// If cache load fails, just use job hosts
			return hostsLoadedMsg{hostNames: jobHosts, err: nil}
		}

		// Merge into unique set
		hostSet := make(map[string]bool)
		for _, h := range jobHosts {
			hostSet[h] = true
		}
		for _, h := range cachedHosts {
			hostSet[h.Name] = true
		}

		// Convert to sorted slice using natural ordering
		var hosts []string
		for h := range hostSet {
			hosts = append(hosts, h)
		}
		naturalSortStrings(hosts)

		return hostsLoadedMsg{hostNames: hosts, err: nil}
	}
}

func (m Model) loadHostSyncTimes() tea.Cmd {
	database := m.database
	return func() tea.Msg {
		times, err := db.LoadHostSyncTimes(database)
		if err != nil {
			return hostSyncTimesLoadedMsg{err: err}
		}
		return hostSyncTimesLoadedMsg{times: times}
	}
}

// requestHostInfoRefresh requests a host info refresh via the sync worker.
func (m Model) requestHostInfoRefresh(hostName string, priority bool) {
	if m.syncWorker != nil {
		m.syncWorker.Request(SyncRequest{
			Host:     hostName,
			Rate:     RateRunning,
			Priority: priority,
		})
	}
}

func (m Model) requestHostSyncPriority(hostName string) {
	if m.syncWorker == nil || hostName == "" {
		return
	}
	m.syncWorker.Request(SyncRequest{
		Host:     hostName,
		Rate:     RateRunning,
		Priority: true,
	})
}

func (m *Model) requestJobCPUTop(job *db.Job) tea.Cmd {
	if job == nil {
		m.jobCPUTopEntries = nil
		m.jobCPUTopError = ""
		m.jobCPUTopRequestedHost = ""
		m.jobCPUTopDataHost = ""
		m.jobCPUTopLoading = false
		return nil
	}
	if m.jobCPUTopDataHost != job.Host {
		m.jobCPUTopEntries = nil
	}
	m.jobCPUTopLoading = true
	m.jobCPUTopError = ""
	m.jobCPUTopRequestedHost = job.Host
	return m.fetchTopProcesses(job.Host, true)
}

func (m *Model) requestHostCPUTop(hostName string) tea.Cmd {
	if hostName == "" {
		m.hostCPUTopEntries = nil
		m.hostCPUTopError = ""
		m.hostCPUTopRequestedHost = ""
		m.hostCPUTopDataHost = ""
		m.hostCPUTopLoading = false
		return nil
	}
	if m.hostCPUTopDataHost != hostName {
		m.hostCPUTopEntries = nil
	}
	m.hostCPUTopLoading = true
	m.hostCPUTopError = ""
	m.hostCPUTopRequestedHost = hostName
	return m.fetchTopProcesses(hostName, false)
}

func (m Model) fetchTopProcesses(host string, jobView bool) tea.Cmd {
	if host == "" {
		return nil
	}
	return func() tea.Msg {
		procs, err := ssh.GetTopProcesses(host, topProcessLimit)
		return cpuTopMsg{
			host:      host,
			processes: procs,
			err:       err,
			jobView:   jobView,
		}
	}
}

func (m Model) startQueue(host string) tea.Cmd {
	return func() tea.Msg {
		runner := queuerunner.NewRunner(host)
		started, err := runner.EnsureStarted("")
		if err != nil {
			return queueStartedMsg{host: host, err: err}
		}
		return queueStartedMsg{host: host, already: !started}
	}
}

// ensureQueueRunnerStartedTUI checks if queue runner is running and starts it if not.
// It also ensures the agent binary is up-to-date before starting.
// Returns (true, nil) if started, (false, nil) if already running, (false, err) on error.
func ensureQueueRunnerStartedTUI(host string) (bool, error) {
	// Deploy agent binary if out of date
	if spec := findHostSpecFunc(host); spec != nil {
		if _, err := ensureAgentUpToDateFunc(host, *spec); err != nil {
			if !errors.Is(err, agentdeploy.ErrAgentNotAvailable) {
				return false, fmt.Errorf("agent deploy failed: %w", err)
			}
			// ErrAgentNotAvailable: binaries not built yet; skip deploy silently.
		}
	}

	// Deploy notify script and build env vars if Slack is configured
	slackWebhook := slack.GetWebhook()
	slack.DeployNotifyScript(host, slackWebhook)
	envVars := slack.BuildRunnerEnvPrefix(slackWebhook)

	runner := queuerunner.NewRunner(host)
	started, err := runner.EnsureStarted(envVars)
	if err != nil {
		return false, fmt.Errorf("queue runner start failed: %w", err)
	}
	return started, nil
}

func (m *Model) cycleHostFilter() {
	hosts := m.hostFilterCandidates()
	switch m.jobHostFilterMode {
	case hostFilterRecent:
		m.jobHostFilterMode = hostFilterAll
		m.jobHostFilterHost = ""
	case hostFilterAll:
		if len(hosts) == 0 {
			m.jobHostFilterMode = hostFilterRecent
			m.jobHostFilterHost = ""
			return
		}
		m.jobHostFilterMode = hostFilterSpecific
		m.jobHostFilterHost = hosts[0]
	case hostFilterSpecific:
		if len(hosts) == 0 {
			m.jobHostFilterMode = hostFilterRecent
			m.jobHostFilterHost = ""
			return
		}
		index := -1
		for i, host := range hosts {
			if host == m.jobHostFilterHost {
				index = i
				break
			}
		}
		if index == -1 || index == len(hosts)-1 {
			m.jobHostFilterMode = hostFilterRecent
			m.jobHostFilterHost = ""
			return
		}
		m.jobHostFilterHost = hosts[index+1]
	default:
		m.jobHostFilterMode = hostFilterRecent
		m.jobHostFilterHost = ""
	}
}

func (m Model) hostFilterCandidates() []string {
	hostSet := make(map[string]struct{})
	for _, host := range m.hosts {
		if host.Name != "" {
			hostSet[host.Name] = struct{}{}
		}
	}
	if len(hostSet) == 0 {
		for _, job := range m.allJobs {
			if job.HasInventoryHost() {
				hostSet[job.Host] = struct{}{}
			}
		}
	}

	hosts := make([]string, 0, len(hostSet))
	for host := range hostSet {
		hosts = append(hosts, host)
	}
	naturalSortStrings(hosts)
	return hosts
}

func (m Model) isHostRecentlySynced(host string) bool {
	if len(m.hostSyncTimes) == 0 {
		return true
	}
	if !m.anyHostRecentlySynced() {
		return true
	}
	last, ok := m.hostSyncTimes[host]
	if !ok || last.IsZero() {
		return false
	}
	return time.Since(last) <= hostRecentSyncWindow
}

func (m Model) anyHostRecentlySynced() bool {
	for _, last := range m.hostSyncTimes {
		if !last.IsZero() && time.Since(last) <= hostRecentSyncWindow {
			return true
		}
	}
	return false
}

func (m Model) deleteHost(hostName string) tea.Cmd {
	database := m.database
	return func() tea.Msg {
		// Check for running or queued jobs on this host
		jobs, err := db.GetJobsByHost(database, hostName)
		if err != nil {
			return hostDeletedMsg{hostName: hostName, err: fmt.Errorf("check jobs: %w", err)}
		}

		var activeJobs []int64
		var jobsToTombstone []int64
		for _, job := range jobs {
			switch job.EffectiveStatus() {
			case db.StatusRunning, db.StatusQueued, db.StatusStarting, db.StatusPaused:
				activeJobs = append(activeJobs, job.ID)
			default:
				// Completed, failed, dead jobs can be tombstoned
				jobsToTombstone = append(jobsToTombstone, job.ID)
			}
		}

		if len(activeJobs) > 0 {
			return hostDeletedMsg{
				hostName: hostName,
				err:      fmt.Errorf("host has %d active jobs (IDs: %v)", len(activeJobs), activeJobs),
			}
		}

		// Tombstone (delete) inactive jobs for this host
		for _, jobID := range jobsToTombstone {
			if err := db.DeleteJob(database, jobID); err != nil {
				return hostDeletedMsg{hostName: hostName, err: fmt.Errorf("tombstone job %d: %w", jobID, err)}
			}
		}

		// Delete the host from cache
		if err := db.DeleteCachedHost(database, hostName); err != nil {
			return hostDeletedMsg{hostName: hostName, err: fmt.Errorf("delete host: %w", err)}
		}

		return hostDeletedMsg{hostName: hostName}
	}
}

func (m Model) jobCountsForHost(host string) (running, queued int) {
	for _, job := range m.allJobs {
		if job.Host != host || job.Tombstoned {
			continue
		}
		switch job.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting, db.StatusPaused:
			running++
		case db.StatusQueued:
			queued++
		}
	}
	return
}

// generateAllHostSummaries triggers summary generation for the next host that needs one
// Only generates one at a time to avoid overwhelming the system
func (m Model) generateAllHostSummaries() tea.Cmd {
	if m.llmGenerator == nil || !m.llmGenerator.IsAvailable() {
		return nil
	}
	// Check if any summary is already being generated
	for _, pending := range m.hostSummaryPending {
		if pending {
			return nil // Wait for current generation to complete
		}
	}
	// Check available RAM before generating (skip if < 1GB available)
	if !hasEnoughRAM(1 * 1024 * 1024 * 1024) {
		return nil
	}
	// Find first host that needs a summary
	for _, host := range m.hosts {
		if cmd := m.maybeGenerateHostSummary(host.Name); cmd != nil {
			return cmd
		}
	}
	return nil
}

// maybeGenerateHostSummary generates a summary if hash changed and rate limit allows
func (m Model) maybeGenerateHostSummary(host string) tea.Cmd {
	// Check rate limit: no more than once per minute
	if lastTime, ok := m.hostSummaryTimes[host]; ok {
		if time.Since(lastTime) < time.Minute {
			return nil
		}
	}
	// Check if already generating
	if m.hostSummaryPending[host] {
		return nil
	}
	// Compute current hash
	newHash := m.computeHostJobsHash(host)
	if oldHash, ok := m.hostSummaryHashes[host]; ok && oldHash == newHash {
		return nil // No change
	}
	// Mark as pending and generate
	m.hostSummaryPending[host] = true
	return m.generateHostSummary(host, newHash)
}

// computeHostJobsHash computes a hash of job IDs and statuses for a host
func (m Model) computeHostJobsHash(host string) string {
	var parts []string
	for _, job := range m.allJobs {
		if job.Host == host && !job.Tombstoned {
			exitCode := ""
			if job.ExitCode != nil {
				exitCode = fmt.Sprintf(":%d", *job.ExitCode)
			}
			parts = append(parts, fmt.Sprintf("%d:%s%s", job.ID, job.EffectiveStatus(), exitCode))
		}
	}
	// Simple hash: join and take first 16 chars of sha256
	h := sha256.New()
	h.Write([]byte(strings.Join(parts, ",")))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// generateHostSummary generates an AI summary for a host
func (m Model) generateHostSummary(host, hash string) tea.Cmd {
	return func() tea.Msg {
		// Gather job data for this host
		var running, queued, completedOK, failed []string
		for _, job := range m.allJobs {
			if job.Host != host || job.Tombstoned {
				continue
			}
			desc := job.EffectiveDescription()
			if len(desc) > 40 {
				desc = desc[:40] + "..."
			}
			switch job.EffectiveStatus() {
			case db.StatusRunning, db.StatusStarting, db.StatusPaused:
				running = append(running, desc)
			case db.StatusQueued:
				queued = append(queued, desc)
			case db.StatusCompleted:
				if job.ExitCode != nil && *job.ExitCode == 0 {
					completedOK = append(completedOK, desc)
				} else {
					failed = append(failed, desc)
				}
			}
		}

		// Build facts for the prompt (simplified format)
		var facts []string
		if len(running) > 0 {
			facts = append(facts, fmt.Sprintf("RUNNING: %s", strings.Join(running, ", ")))
		}
		if len(queued) > 0 {
			if len(queued) > 3 {
				facts = append(facts, fmt.Sprintf("QUEUED: %d jobs", len(queued)))
			} else {
				facts = append(facts, fmt.Sprintf("QUEUED: %s", strings.Join(queued, ", ")))
			}
		}
		if len(completedOK) > 0 {
			if len(completedOK) > 3 {
				completedOK = completedOK[:3]
			}
			facts = append(facts, fmt.Sprintf("OK: %s", strings.Join(completedOK, ", ")))
		}
		if len(failed) > 0 {
			if len(failed) > 3 {
				facts = append(facts, fmt.Sprintf("FAILED: %d jobs including %s", len(failed), failed[0]))
			} else {
				facts = append(facts, fmt.Sprintf("FAILED: %s", strings.Join(failed, ", ")))
			}
		}

		// No activity at all
		if len(facts) == 0 {
			return hostSummaryGeneratedMsg{host: host, summary: "No recent activity", hash: hash}
		}

		// Generate via ollama
		prompt := fmt.Sprintf(`Rewrite as a status summary (1-2 sentences). Be specific about what's running and any failures.

%s

Status:`, strings.Join(facts, "\n"))

		summary, err := m.llmGenerator.GenerateText(prompt)
		if err != nil {
			return hostSummaryGeneratedMsg{host: host, err: err}
		}
		return hostSummaryGeneratedMsg{host: host, summary: summary, hash: hash}
	}
}

// hasEnoughRAM checks if the system has at least minBytes of available RAM
func hasEnoughRAM(minBytes uint64) bool {
	if runtime.GOOS == "darwin" {
		// Use vm_stat on macOS
		out, err := exec.Command("vm_stat").Output()
		if err != nil {
			return true // Assume OK if we can't check
		}
		// Parse pages - include free, inactive, and speculative (all reclaimable)
		var freePages, inactivePages, speculativePages uint64
		lines := strings.Split(string(out), "\n")
		for _, line := range lines {
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				numStr := strings.TrimSuffix(parts[len(parts)-1], ".")
				pages, err := strconv.ParseUint(numStr, 10, 64)
				if err != nil {
					continue
				}
				switch {
				case strings.HasPrefix(line, "Pages free:"):
					freePages = pages
				case strings.HasPrefix(line, "Pages inactive:"):
					inactivePages = pages
				case strings.HasPrefix(line, "Pages speculative:"):
					speculativePages = pages
				}
			}
		}
		// Available = free + inactive + speculative (all can be reclaimed)
		availablePages := freePages + inactivePages + speculativePages
		availableBytes := availablePages * 4096
		return availableBytes >= minBytes
	}
	return true // Default to allowing on other platforms
}
