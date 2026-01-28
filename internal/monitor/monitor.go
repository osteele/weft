package monitor

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/hostinfo"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/queuerunner"
	"github.com/osteele/remote-jobs/internal/remote"
	"github.com/osteele/remote-jobs/internal/slack"
)

// Default intervals for background operations.
const (
	DefaultSyncActiveInterval  = 15 * time.Second
	DefaultSyncIdleInterval    = 60 * time.Second
	DefaultHostRefreshInterval = 30 * time.Second
	DefaultHostCacheDuration   = 24 * time.Hour
	dbChangeDebounceInterval   = 200 * time.Millisecond
)

// Config controls monitor behavior.
type Config struct {
	SyncActiveInterval  time.Duration
	SyncIdleInterval    time.Duration
	HostRefreshInterval time.Duration
	HostCacheDuration   time.Duration
}

// DefaultConfig returns the default monitor configuration.
func DefaultConfig() Config {
	return Config{
		SyncActiveInterval:  DefaultSyncActiveInterval,
		SyncIdleInterval:    DefaultSyncIdleInterval,
		HostRefreshInterval: DefaultHostRefreshInterval,
		HostCacheDuration:   DefaultHostCacheDuration,
	}
}

// EventType is the kind of event emitted by the monitor.
type EventType int

const (
	EventJobsRefreshed EventType = iota
	EventHostsLoaded
	EventHostInfoUpdated
	EventHostSyncTimesLoaded
	EventSyncCompleted
	EventError
)

// Event carries monitor updates.
type Event struct {
	Type EventType
	Err  error

	Jobs            []*db.Job
	JobDependencies map[int64]string
	HostNames       []string
	Host            *hostinfo.Host
	HostSyncTimes   map[string]time.Time
	SyncResult      SyncResult
}

// SyncResult captures the outcome of a background sync.
type SyncResult struct {
	Updated           int
	QueuesStarted     []string
	HostsSynced       []string
	QueueRunnerErrors []string
}

// Monitor coordinates background refresh and sync work.
type Monitor struct {
	db     *sql.DB
	config Config

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
	subsMu   sync.Mutex
	subs     map[chan Event]struct{}

	mu                      sync.RWMutex
	jobs                    []*db.Job
	jobDependencies         map[int64]string
	hosts                   []*hostinfo.Host
	hostSyncTimes           map[string]time.Time
	lastHostSyncTimes       map[string]time.Time
	hostsQueriedThisRun     map[string]bool
	syncForceAll            bool
	syncPendingAfterCurrent bool
	syncing                 bool
	lastSyncTime            time.Time

	dbWatcher         *fsnotify.Watcher
	dbWatcherTargets  map[string]struct{}
	dbRefreshDebounce *time.Timer
	dbSyncDebounce    *time.Timer
	started           bool
	stopped           bool
}

// New returns a monitor for the given database.
func New(database *sql.DB, cfg Config) *Monitor {
	return &Monitor{
		db:                  database,
		config:              cfg,
		stop:                make(chan struct{}),
		subs:                make(map[chan Event]struct{}),
		hostSyncTimes:       make(map[string]time.Time),
		lastHostSyncTimes:   make(map[string]time.Time),
		hostsQueriedThisRun: make(map[string]bool),
	}
}

// Config returns the monitor configuration.
func (m *Monitor) Config() Config {
	return m.config
}

// Events returns a new subscription channel for monitor events.
func (m *Monitor) Events() <-chan Event {
	ch := make(chan Event, 16)
	m.subsMu.Lock()
	m.subs[ch] = struct{}{}
	m.subsMu.Unlock()
	return ch
}

// Start launches background tasks.
func (m *Monitor) Start() {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()

	m.refreshJobs()
	m.refreshHosts()
	m.refreshHostSyncTimes()

	m.wg.Add(1)
	go m.runDBWatcher()

	m.wg.Add(1)
	go m.runSyncTicker()

	m.wg.Add(1)
	go m.runHostRefreshTicker()
}

// Stop stops background tasks.
func (m *Monitor) Stop() {
	m.stopOnce.Do(func() {
		m.mu.Lock()
		m.stopped = true
		if m.dbRefreshDebounce != nil {
			m.dbRefreshDebounce.Stop()
			m.dbRefreshDebounce = nil
		}
		if m.dbSyncDebounce != nil {
			m.dbSyncDebounce.Stop()
			m.dbSyncDebounce = nil
		}
		m.mu.Unlock()
		close(m.stop)
	})
	m.wg.Wait()
	if m.dbWatcher != nil {
		_ = m.dbWatcher.Close()
	}
	m.subsMu.Lock()
	for ch := range m.subs {
		close(ch)
	}
	m.subs = make(map[chan Event]struct{})
	m.subsMu.Unlock()
}

// Jobs returns a snapshot of the latest jobs list.
func (m *Monitor) Jobs() []*db.Job {
	m.mu.RLock()
	defer m.mu.RUnlock()
	jobs := make([]*db.Job, len(m.jobs))
	copy(jobs, m.jobs)
	return jobs
}

// Hosts returns a snapshot of the latest host info.
func (m *Monitor) Hosts() []*hostinfo.Host {
	m.mu.RLock()
	defer m.mu.RUnlock()
	hosts := make([]*hostinfo.Host, 0, len(m.hosts))
	for _, host := range m.hosts {
		if host == nil {
			continue
		}
		copyHost := *host
		if len(host.GPUs) > 0 {
			copyHost.GPUs = append([]hostinfo.GPUInfo(nil), host.GPUs...)
		}
		if len(host.RunningJobs) > 0 {
			copyHost.RunningJobs = append([]hostinfo.HostRunningJob(nil), host.RunningJobs...)
		}
		hosts = append(hosts, &copyHost)
	}
	return hosts
}

// HostSyncTimes returns a snapshot of host sync timestamps.
func (m *Monitor) HostSyncTimes() map[string]time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	copyTimes := make(map[string]time.Time, len(m.hostSyncTimes))
	for key, value := range m.hostSyncTimes {
		copyTimes[key] = value
	}
	return copyTimes
}

// JobDependencies returns a snapshot of job dependency info.
func (m *Monitor) JobDependencies() map[int64]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	deps := make(map[int64]string, len(m.jobDependencies))
	for key, value := range m.jobDependencies {
		deps[key] = value
	}
	return deps
}

// RefreshJobs triggers an immediate job refresh.
func (m *Monitor) RefreshJobs() {
	go m.refreshJobs()
}

// RefreshHosts triggers an immediate host list refresh.
func (m *Monitor) RefreshHosts() {
	go m.refreshHosts()
}

// RefreshHostSyncTimes triggers an immediate host sync time refresh.
func (m *Monitor) RefreshHostSyncTimes() {
	go m.refreshHostSyncTimes()
}

// RefreshHostInfo triggers a host info refresh.
func (m *Monitor) RefreshHostInfo(hostName string) {
	go m.refreshHostInfo(hostName)
}

// RequestSync requests a background sync.
func (m *Monitor) RequestSync(forceAll bool) {
	go m.requestSync(forceAll)
}

func (m *Monitor) runSyncTicker() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.config.SyncActiveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			go m.requestSync(false)
		case <-m.stop:
			return
		}
	}
}

func (m *Monitor) runHostRefreshTicker() {
	defer m.wg.Done()
	ticker := time.NewTicker(m.config.HostRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			go m.refreshHostsForStatus()
		case <-m.stop:
			return
		}
	}
}

func (m *Monitor) runDBWatcher() {
	defer m.wg.Done()
	dbFile := db.Path()
	if dbFile == "" {
		return
	}
	dir := filepath.Dir(dbFile)
	targets := map[string]struct{}{}
	addTarget := func(name string) {
		if name == "" {
			return
		}
		targets[filepath.Clean(filepath.Join(dir, name))] = struct{}{}
	}
	base := filepath.Base(dbFile)
	addTarget(base)
	addTarget(base + "-wal")
	addTarget(base + "-shm")

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		m.emit(Event{Type: EventError, Err: err})
		return
	}
	m.dbWatcher = watcher
	m.dbWatcherTargets = targets
	if err := watcher.Add(dir); err != nil {
		m.emit(Event{Type: EventError, Err: err})
		return
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if !isWatchedDBFile(event.Name, targets) {
				continue
			}
			if event.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			m.scheduleDBRefresh()
			m.scheduleDBSync()
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			m.emit(Event{Type: EventError, Err: err})
		case <-m.stop:
			return
		}
	}
}

func (m *Monitor) scheduleDBRefresh() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	if m.dbRefreshDebounce != nil {
		return
	}
	m.dbRefreshDebounce = time.AfterFunc(dbChangeDebounceInterval, func() {
		m.mu.Lock()
		m.dbRefreshDebounce = nil
		m.mu.Unlock()
		m.refreshJobs()
		m.refreshHosts()
	})
}

func (m *Monitor) scheduleDBSync() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}
	if m.dbSyncDebounce != nil {
		return
	}
	interval := m.config.SyncActiveInterval
	m.dbSyncDebounce = time.AfterFunc(interval, func() {
		m.mu.Lock()
		m.dbSyncDebounce = nil
		m.mu.Unlock()
		m.requestSync(true)
	})
}

func (m *Monitor) emit(event Event) {
	m.subsMu.Lock()
	defer m.subsMu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- event:
		default:
		}
	}
}

func (m *Monitor) refreshJobs() {
	jobs, err := db.ListJobs(m.db, "", "", 1000, nil, "")
	if err != nil {
		m.emit(Event{Type: EventJobsRefreshed, Err: err})
		return
	}
	deps, _ := db.GetJobDependencyInfo(m.db)

	m.mu.Lock()
	m.jobs = jobs
	m.jobDependencies = deps
	m.mu.Unlock()

	m.emit(Event{
		Type:            EventJobsRefreshed,
		Jobs:            jobs,
		JobDependencies: deps,
	})
}

func (m *Monitor) refreshHosts() {
	jobHosts, err := db.ListUniqueHosts(m.db)
	if err != nil {
		m.emit(Event{Type: EventHostsLoaded, Err: err})
		return
	}

	cachedHosts, err := db.LoadAllCachedHosts(m.db)
	if err != nil {
		m.emit(Event{Type: EventHostsLoaded, HostNames: jobHosts})
		return
	}

	hostSet := make(map[string]bool)
	for _, h := range jobHosts {
		hostSet[h] = true
	}
	for _, h := range cachedHosts {
		hostSet[h.Name] = true
	}

	var hosts []string
	for h := range hostSet {
		hosts = append(hosts, h)
	}
	naturalSortStrings(hosts)

	m.mu.Lock()
	m.hosts = nil
	for _, name := range hosts {
		var host *hostinfo.Host
		cachedInfo, err := db.LoadCachedHostInfo(m.db, name)
		if err == nil && cachedInfo != nil {
			host = hostinfo.HostFromCachedInfo(cachedInfo)
			cacheAge := time.Since(time.Unix(cachedInfo.LastUpdated, 0))
			if cacheAge > m.config.HostCacheDuration {
				host.Status = hostinfo.HostStatusChecking
			} else {
				// Cache is fresh, assume host is still online while we refresh
				host.Status = hostinfo.HostStatusOnline
			}
			// Always refresh to get current dynamic metrics (CPU load, RAM usage)
			go m.refreshHostInfo(name)
		} else {
			host = &hostinfo.Host{
				Name:   name,
				Status: hostinfo.HostStatusChecking,
			}
			go m.refreshHostInfo(name)
		}
		m.hosts = append(m.hosts, host)
	}
	m.mu.Unlock()

	m.emit(Event{Type: EventHostsLoaded, HostNames: hosts})
}

func (m *Monitor) refreshHostSyncTimes() {
	times, err := db.LoadHostSyncTimes(m.db)
	if err != nil {
		m.emit(Event{Type: EventHostSyncTimesLoaded, Err: err})
		return
	}
	m.mu.Lock()
	m.hostSyncTimes = times
	m.mu.Unlock()
	m.emit(Event{Type: EventHostSyncTimesLoaded, HostSyncTimes: times})
}

func (m *Monitor) refreshHostInfo(hostName string) {
	host := &hostinfo.Host{
		Name:   hostName,
		Status: hostinfo.HostStatusChecking,
	}

	stdout, stderr, err := remote.RunWithTimeout(hostName, hostinfo.HostInfoCommand, 10*time.Second)
	if err != nil {
		host.Status = hostinfo.HostStatusOffline
		host.Error = strings.TrimSpace(stderr)
		if host.Error == "" {
			host.Error = err.Error()
		}
		if cachedInfo, loadErr := db.LoadCachedHostInfo(m.db, hostName); loadErr == nil && cachedInfo != nil {
			cachedHost := hostinfo.HostFromCachedInfo(cachedInfo)
			hostinfo.UpdateHostWithCachedStatic(host, cachedHost)
		}
		m.updateHostInfo(host)
		return
	}

	host = hostinfo.ParseHostInfo(stdout)
	host.Name = hostName

	if cachedInfo := hostinfo.CachedInfoFromHost(host); cachedInfo != nil {
		_ = db.SaveCachedHostInfo(m.db, cachedInfo)
	}

	m.updateHostInfo(host)
}

func (m *Monitor) updateHostInfo(host *hostinfo.Host) {
	m.mu.Lock()
	defer m.mu.Unlock()

	found := false
	for i, h := range m.hosts {
		if h != nil && h.Name == host.Name {
			m.hosts[i] = host
			found = true
			break
		}
	}
	if !found {
		m.hosts = append(m.hosts, host)
	}
	m.hostsQueriedThisRun[host.Name] = true
	m.emit(Event{Type: EventHostInfoUpdated, Host: host})
}

func (m *Monitor) refreshHostsForStatus() {
	hostsWithRunningJobs := m.runningHosts()

	m.mu.RLock()
	hosts := append([]*hostinfo.Host(nil), m.hosts...)
	queried := make(map[string]bool, len(m.hostsQueriedThisRun))
	for key, value := range m.hostsQueriedThisRun {
		queried[key] = value
	}
	m.mu.RUnlock()

	for _, host := range hosts {
		if host == nil {
			continue
		}
		needsRefresh := (!queried[host.Name] || host.Status == hostinfo.HostStatusOnline)
		hasRunningJobs := hostsWithRunningJobs[host.Name]
		isOnline := host.Status == hostinfo.HostStatusOnline
		if needsRefresh || hasRunningJobs || isOnline {
			m.refreshHostInfo(host.Name)
		}
	}
}

func (m *Monitor) runningHosts() map[string]bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	hosts := make(map[string]bool)
	for _, job := range m.jobs {
		if job == nil {
			continue
		}
		if job.Status == db.StatusRunning || job.Status == db.StatusStarting {
			hosts[job.Host] = true
		}
	}
	return hosts
}

func (m *Monitor) requestSync(forceAll bool) {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	if m.syncing {
		m.syncPendingAfterCurrent = true
		if forceAll {
			m.syncForceAll = true
		}
		m.mu.Unlock()
		return
	}
	if forceAll {
		m.syncForceAll = true
	}
	m.syncing = true
	m.mu.Unlock()

	result := m.performBackgroundSync(forceAll)
	m.mu.Lock()
	m.syncing = false
	m.lastSyncTime = time.Now()
	pending := m.syncPendingAfterCurrent
	forceNext := m.syncForceAll
	m.syncPendingAfterCurrent = false
	m.syncForceAll = false
	m.mu.Unlock()

	m.emit(Event{
		Type:       EventSyncCompleted,
		SyncResult: result,
	})
	if result.Updated > 0 {
		m.refreshJobs()
	}
	if pending {
		m.requestSync(forceNext)
	}
}

func (m *Monitor) performBackgroundSync(forceAll bool) SyncResult {
	var result SyncResult

	hosts, err := db.ListUniqueHosts(m.db)
	if err != nil {
		m.emit(Event{Type: EventSyncCompleted, Err: err})
		return result
	}

	now := time.Now()
	activeJobsByHost := make(map[string][]*db.Job)
	activeHost := make(map[string]bool)
	for _, host := range hosts {
		jobs, err := db.ListActiveJobs(m.db, host)
		if err != nil {
			continue
		}
		activeJobsByHost[host] = jobs
		activeHost[host] = len(jobs) > 0
	}

	hostsToSync := make([]string, 0, len(hosts))
	for _, host := range hosts {
		interval := m.config.SyncIdleInterval
		if activeHost[host] {
			interval = m.config.SyncActiveInterval
		}
		last := m.lastHostSyncTimes[host]
		due := forceAll || last.IsZero() || now.Sub(last) >= interval
		if due {
			hostsToSync = append(hostsToSync, host)
		}
	}

	if len(hostsToSync) == 0 {
		return result
	}

	tombstonedJobs, _ := db.GetTombstonedActiveJobs(m.db)
	tombstonedByHost := map[string][]*db.Job{}
	for _, job := range tombstonedJobs {
		if job == nil {
			continue
		}
		tombstonedByHost[job.Host] = append(tombstonedByHost[job.Host], job)
	}

	for _, host := range hostsToSync {
		hostSynced := false
		syncOpts := ops.DefaultSyncOptions()
		for _, job := range activeJobsByHost[host] {
			syncResult, err := ops.SyncJobQuick(m.db, job, syncOpts)
			if err != nil {
				oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
					oplog.WithDetail("sync-quick-error"),
					oplog.WithError(err))
				continue
			}
			if syncResult.HostContacted {
				hostSynced = true
			}
			if syncResult.Updated {
				result.Updated++
			}
		}

		if drafts, err := db.ListDraftJobsPendingSync(m.db, host); err == nil {
			for _, job := range drafts {
				syncResult, err := ops.SyncDraftJob(m.db, job, syncOpts)
				if err != nil {
					oplog.LogJob(oplog.OpJobSync, job.ID, job.Host,
						oplog.WithDetail("sync-draft-error"),
						oplog.WithError(err))
					continue
				}
				if syncResult.HostContacted {
					hostSynced = true
				}
				if syncResult.Updated {
					result.Updated++
				}
			}
		}

		for _, job := range tombstonedByHost[host] {
			if job == nil {
				continue
			}
			if killTombstonedJob(m.db, job) {
				result.Updated++
			}
		}

		queueRunnerCount := 0
		for _, job := range activeJobsByHost[host] {
			if job == nil {
				continue
			}
			if job.UsesQueueRunner() && (job.Status == db.StatusQueued || job.Status == db.StatusRunning || job.Status == db.StatusStarting) {
				queueRunnerCount++
			}
		}
		if queueRunnerCount > 0 {
			started, err := ensureQueueRunnerStarted(host)
			if err != nil {
				if !remote.IsConnectionError(err.Error()) {
					result.QueueRunnerErrors = append(result.QueueRunnerErrors, fmt.Sprintf("%s: %v", host, err))
				}
			} else if started {
				result.QueuesStarted = append(result.QueuesStarted, host)
			}
		}
		if hostSynced {
			result.HostsSynced = append(result.HostsSynced, host)
			now := time.Now()
			m.mu.Lock()
			m.lastHostSyncTimes[host] = now
			m.hostSyncTimes[host] = now
			m.mu.Unlock()
			_ = db.RecordHostSync(m.db, host, now)
		}
	}

	if len(result.HostsSynced) > 0 {
		m.emit(Event{
			Type:          EventHostSyncTimesLoaded,
			HostSyncTimes: m.HostSyncTimes(),
		})
	}

	return result
}

// ensureQueueRunnerStarted checks if queue runner is running and starts it if not.
func ensureQueueRunnerStarted(host string) (bool, error) {
	// Check if jq is available (required for queue runner)
	jqAvailable, err := queuerunner.CheckJqAvailable(host)
	if err == nil && !jqAvailable {
		return false, fmt.Errorf("jq is required but not installed on %s", host)
	}

	slackWebhook := slack.GetWebhook()
	slack.DeployNotifyScript(host, slackWebhook)
	envVars := slack.BuildRunnerEnvPrefix(slackWebhook)

	runner := queuerunner.NewRunner(host)
	return runner.EnsureStarted(envVars)
}

// killTombstonedJob kills a job that was tombstoned locally but may still be running remotely.
func killTombstonedJob(database *sql.DB, job *db.Job) bool {
	if job.Status == db.StatusQueued {
		if err := ops.CancelRemoteJob(job, 5*time.Second); err != nil {
			return false
		}
		_ = db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusDead)
		return true
	}

	if err := ops.CancelRemoteJob(job, 5*time.Second); err == nil {
		_ = db.ClearPendingAndUpdateStatus(database, job.ID, db.StatusDead)
		return true
	}
	return false
}

func isWatchedDBFile(name string, targets map[string]struct{}) bool {
	if name == "" {
		return false
	}
	clean := filepath.Clean(name)
	_, ok := targets[clean]
	return ok
}

// naturalSortStrings sorts strings using Finder-style ordering (e.g., host2 < host10).
func naturalSortStrings(s []string) {
	sort.Slice(s, func(i, j int) bool {
		return naturalLess(s[i], s[j])
	})
}

func naturalLess(a, b string) bool {
	aParts := splitIntoSegments(a)
	bParts := splitIntoSegments(b)

	minLen := len(aParts)
	if len(bParts) < minLen {
		minLen = len(bParts)
	}

	for i := 0; i < minLen; i++ {
		aSeg := aParts[i]
		bSeg := bParts[i]
		aNum, aIsNum := parseNumber(aSeg)
		bNum, bIsNum := parseNumber(bSeg)

		if aIsNum && bIsNum {
			if aNum != bNum {
				return aNum < bNum
			}
		} else {
			aLower := strings.ToLower(aSeg)
			bLower := strings.ToLower(bSeg)
			if aLower != bLower {
				return aLower < bLower
			}
		}
	}

	if len(aParts) != len(bParts) {
		return len(aParts) < len(bParts)
	}
	return a < b
}

func splitIntoSegments(s string) []string {
	var segments []string
	var current strings.Builder

	for _, r := range s {
		isDigit := r >= '0' && r <= '9'
		isAlpha := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')

		if current.Len() == 0 {
			current.WriteRune(r)
		} else {
			lastRunes := []rune(current.String())
			lastRune := lastRunes[len(lastRunes)-1]
			lastIsDigit := lastRune >= '0' && lastRune <= '9'
			lastIsAlpha := (lastRune >= 'a' && lastRune <= 'z') || (lastRune >= 'A' && lastRune <= 'Z')

			if (isDigit && lastIsDigit) || (isAlpha && lastIsAlpha) {
				current.WriteRune(r)
			} else {
				segments = append(segments, current.String())
				current.Reset()
				current.WriteRune(r)
			}
		}
	}

	if current.Len() > 0 {
		segments = append(segments, current.String())
	}

	return segments
}

func parseNumber(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	num := 0
	for _, r := range s {
		num = num*10 + int(r-'0')
	}
	return num, true
}
