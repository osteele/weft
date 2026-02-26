package web

import (
	"context"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
	"github.com/osteele/weft/internal/logfiles"
	"github.com/osteele/weft/internal/monitor"
	"github.com/osteele/weft/internal/progress"
	"github.com/osteele/weft/internal/ssh"
)

type Config struct {
	Port int
}

type Server struct {
	monitor *monitor.Monitor
	addr    string
	server  *http.Server
	tmpl    *template.Template

	mu            sync.RWMutex
	jobs          []*db.Job
	hosts         []*hostinfo.Host
	hostSyncTimes map[string]time.Time
	lastUpdated   time.Time
	jobProgress   map[int64]*progress.Progress

	stopCh chan struct{}
}

func NewServer(monitor *monitor.Monitor, cfg Config) (*Server, error) {
	if cfg.Port == 0 {
		cfg.Port = 8127
	}
	addr := fmt.Sprintf("127.0.0.1:%d", cfg.Port)
	tmpl, err := template.New("index").Funcs(template.FuncMap{
		"formatStatus": formatJobStatus,
		"formatTime":   formatJobTime,
		"queryWith":    queryWith,
	}).Parse(indexTemplate)
	if err != nil {
		return nil, err
	}

	return &Server{
		monitor:       monitor,
		addr:          addr,
		tmpl:          tmpl,
		hostSyncTimes: make(map[string]time.Time),
		jobProgress:   make(map[int64]*progress.Progress),
		stopCh:        make(chan struct{}),
	}, nil
}

func (s *Server) Start() (string, error) {
	if s.monitor == nil {
		return "", fmt.Errorf("monitor is required")
	}
	s.captureSnapshot()
	go s.consumeEvents()
	go s.progressFetcher()

	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return "", err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/cluster", s.handleCluster)
	mux.HandleFunc("/api/hosts", s.handleAPIHosts)
	mux.HandleFunc("/api/coordinator", s.handleAPICoordinator)
	mux.HandleFunc("/api/oplog", s.handleAPIOplog)
	s.server = &http.Server{
		Handler: mux,
	}

	go func() {
		_ = s.server.Serve(listener)
	}()

	return "http://" + listener.Addr().String(), nil
}

func (s *Server) Stop(ctx context.Context) error {
	close(s.stopCh)
	if s.server == nil {
		return nil
	}
	return s.server.Shutdown(ctx)
}

func (s *Server) captureSnapshot() {
	if s.monitor == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs = s.monitor.Jobs()
	s.hosts = s.monitor.Hosts()
	s.hostSyncTimes = s.monitor.HostSyncTimes()
	s.lastUpdated = time.Now()
}

func (s *Server) consumeEvents() {
	for event := range s.monitor.Events() {
		switch event.Type {
		case monitor.EventJobsRefreshed:
			s.mu.Lock()
			if event.Err == nil {
				s.jobs = event.Jobs
				s.lastUpdated = time.Now()
			}
			s.mu.Unlock()
		case monitor.EventHostsLoaded:
			s.mu.Lock()
			if event.Err == nil {
				s.hosts = s.monitor.Hosts()
			}
			s.mu.Unlock()
		case monitor.EventHostInfoUpdated:
			s.mu.Lock()
			if event.Host != nil {
				s.updateHost(event.Host)
			}
			s.mu.Unlock()
		case monitor.EventHostSyncTimesLoaded:
			s.mu.Lock()
			if event.Err == nil && event.HostSyncTimes != nil {
				s.hostSyncTimes = event.HostSyncTimes
			}
			s.mu.Unlock()
		}
	}
}

func (s *Server) updateHost(host *hostinfo.Host) {
	found := false
	for i, h := range s.hosts {
		if h != nil && h.Name == host.Name {
			s.hosts[i] = host
			found = true
			break
		}
	}
	if !found {
		s.hosts = append(s.hosts, host)
	}
}

type viewOption struct {
	ID    string
	Label string
}

var viewOptions = []viewOption{
	{ID: "recent", Label: "Recent"},
	{ID: "active", Label: "Active"},
	{ID: "succeeded", Label: "Succeeded"},
	{ID: "failed", Label: "Failed"},
	{ID: "all", Label: "All"},
}

type hostFilterOption struct {
	ID    string
	Label string
}

type pageData struct {
	Title          string
	Views          []viewOption
	SelectedView   string
	HostFilters    []hostFilterOption
	SelectedHost   string
	HostSummaries  []hostSummary
	Jobs           []jobRow
	ShowGPU        bool
	ShowProject    bool
	RefreshSeconds int
	LastUpdated    string
	QueryParams    url.Values
}

type hostSummary struct {
	Name   string
	Status string
	CPU    string
	RAM    string
	Style  string
	Dimmed bool
}

type jobRow struct {
	ID          int64
	Host        string
	Project     string
	Status      string
	StatusClass string
	Time        string
	GPU         string
	Description string
	Tags        []string
	TooltipHTML template.HTML
}

func (s *Server) handleCluster(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(clusterTemplate))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// Always get fresh data from monitor to ensure we have latest metrics
	var jobs []*db.Job
	var hosts []*hostinfo.Host
	var hostSyncTimes map[string]time.Time
	var lastUpdated time.Time

	if s.monitor != nil {
		jobs = s.monitor.Jobs()
		hosts = s.monitor.Hosts()
		hostSyncTimes = s.monitor.HostSyncTimes()
		lastUpdated = time.Now()
	} else {
		s.mu.RLock()
		jobs = append([]*db.Job(nil), s.jobs...)
		hosts = append([]*hostinfo.Host(nil), s.hosts...)
		hostSyncTimes = make(map[string]time.Time, len(s.hostSyncTimes))
		for key, value := range s.hostSyncTimes {
			hostSyncTimes[key] = value
		}
		lastUpdated = s.lastUpdated
		s.mu.RUnlock()
	}

	selectedView := normalizeView(r.URL.Query().Get("view"))
	selectedHost := normalizeHostFilter(r.URL.Query().Get("host"))

	filteredJobs := filterJobsByView(jobs, selectedView)
	filteredJobs = filterJobsByHost(filteredJobs, selectedHost, hostSyncTimes)
	sortJobsForView(filteredJobs, selectedView)

	showGPU := false
	showProject := false
	rows := make([]jobRow, 0, len(filteredJobs))
	for _, job := range filteredJobs {
		gpu := job.GetGPU()
		if gpu != "" {
			showGPU = true
		}
		if gpu == "" {
			gpu = "—"
		}
		if job.Project != "" {
			showProject = true
		}
		// Get status with progress if available
		status := formatJobStatus(job)
		if job.Status == db.StatusRunning {
			if prog := s.getJobProgress(job.ID); prog != nil {
				pct := prog.DisplayPercent()
				if pct >= 0 {
					status = fmt.Sprintf("● %d%%", pct)
				}
			}
		}
		rows = append(rows, jobRow{
			ID:          job.ID,
			Host:        job.Host,
			Project:     job.Project,
			Status:      status,
			StatusClass: jobStatusClass(job),
			Time:        formatJobTime(job),
			GPU:         gpu,
			Description: job.EffectiveDescription(),
			Tags:        job.Tags,
			TooltipHTML: buildJobTooltipHTML(job),
		})
	}

	hostFilters := buildHostFilters(hosts)
	hostSummaries := buildHostSummaries(hosts, hostSyncTimes)

	refreshSeconds := refreshIntervalSeconds(jobs, s.monitor)
	data := pageData{
		Title:          "weft",
		Views:          viewOptions,
		SelectedView:   selectedView,
		HostFilters:    hostFilters,
		SelectedHost:   selectedHost,
		HostSummaries:  hostSummaries,
		Jobs:           rows,
		ShowGPU:        showGPU,
		ShowProject:    showProject,
		RefreshSeconds: refreshSeconds,
		LastUpdated:    formatLastUpdated(lastUpdated),
		QueryParams:    r.URL.Query(),
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tmpl.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func normalizeView(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, option := range viewOptions {
		if option.ID == value {
			return value
		}
	}
	return "recent"
}

func normalizeHostFilter(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "recent"
	}
	switch strings.ToLower(value) {
	case "recent", "all":
		return strings.ToLower(value)
	default:
		return value
	}
}

func queryWith(values url.Values, key, value string) string {
	next := url.Values{}
	for k, v := range values {
		next[k] = append([]string(nil), v...)
	}
	if value == "" {
		next.Del(key)
	} else {
		next.Set(key, value)
	}
	encoded := next.Encode()
	if encoded == "" {
		return ""
	}
	return "?" + encoded
}

func buildHostFilters(hosts []*hostinfo.Host) []hostFilterOption {
	hostSet := make(map[string]struct{})
	for _, host := range hosts {
		if host == nil || host.Name == "" {
			continue
		}
		hostSet[host.Name] = struct{}{}
	}
	names := make([]string, 0, len(hostSet))
	for name := range hostSet {
		names = append(names, name)
	}
	sort.Strings(names)

	filters := []hostFilterOption{
		{ID: "recent", Label: "Synced <2d"},
		{ID: "all", Label: "All"},
	}
	for _, name := range names {
		filters = append(filters, hostFilterOption{ID: name, Label: name})
	}
	return filters
}

func buildHostSummaries(hosts []*hostinfo.Host, hostSyncTimes map[string]time.Time) []hostSummary {
	const staleThreshold = 5 * time.Minute
	summaries := make([]hostSummary, 0, len(hosts))
	for _, host := range hosts {
		if host == nil {
			continue
		}
		// Skip hosts that haven't been synced recently (e.g. decommissioned hosts)
		if !isHostRecentlySynced(host.Name, hostSyncTimes) {
			continue
		}
		statusClass := hostStatusClass(host.Status)

		// Dim hosts that are offline or have stale data
		isStale := !host.LastCheck.IsZero() && time.Since(host.LastCheck) > staleThreshold
		dimmed := host.Status != hostinfo.HostStatusOnline || isStale

		// Show metrics for online hosts even if data is stale (will be dimmed visually).
		// Only hide metrics for offline/checking hosts where we have no valid data.
		cpuText := "--"
		ramText := "--"
		if host.Status == hostinfo.HostStatusOnline {
			if pct, ok := hostinfo.HostCPULoadPercent(host); ok {
				cpuText = fmt.Sprintf("%d%%", pct)
			}
			if pct, ok := hostinfo.HostMemUsagePercent(host); ok {
				ramText = fmt.Sprintf("%d%%", pct)
			}
		}
		summaries = append(summaries, hostSummary{
			Name:   host.Name,
			Status: statusLabel(host.Status),
			CPU:    cpuText,
			RAM:    ramText,
			Style:  statusClass,
			Dimmed: dimmed,
		})
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].Name < summaries[j].Name })
	return summaries
}

func statusLabel(status hostinfo.HostStatus) string {
	switch status {
	case hostinfo.HostStatusOnline:
		return "online"
	case hostinfo.HostStatusChecking:
		return "checking"
	case hostinfo.HostStatusOffline:
		return "offline"
	default:
		return "unknown"
	}
}

func hostStatusClass(status hostinfo.HostStatus) string {
	switch status {
	case hostinfo.HostStatusOnline:
		return "status-online"
	case hostinfo.HostStatusChecking:
		return "status-checking"
	case hostinfo.HostStatusOffline:
		return "status-offline"
	default:
		return "status-unknown"
	}
}

func refreshIntervalSeconds(jobs []*db.Job, mon *monitor.Monitor) int {
	if mon == nil {
		return int(monitor.DefaultSyncActiveInterval.Seconds())
	}
	active := false
	for _, job := range jobs {
		if job == nil {
			continue
		}
		switch job.Status {
		case db.StatusRunning, db.StatusStarting, db.StatusQueued, db.StatusDraft:
			active = true
		}
		if active {
			break
		}
	}
	if active {
		return int(mon.Config().SyncActiveInterval.Seconds())
	}
	return int(mon.Config().SyncIdleInterval.Seconds())
}

func formatLastUpdated(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format("15:04:05")
}

// jobStatusClass returns a CSS class name for the job status
func jobStatusClass(job *db.Job) string {
	if job == nil {
		return ""
	}
	switch job.Status {
	case db.StatusRunning, db.StatusStarting:
		return "job-running"
	case db.StatusPaused:
		return "job-paused"
	case db.StatusCompleted:
		if job.ExitCode != nil && *job.ExitCode != 0 {
			return "job-failed"
		}
		return "job-completed"
	case db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
		return "job-dead"
	case db.StatusQueued:
		return "job-pending"
	case db.StatusDraft:
		return "job-draft"
	default:
		return ""
	}
}

// buildJobTooltipHTML creates an HTML tooltip with job details in columnar format
func buildJobTooltipHTML(job *db.Job) template.HTML {
	if job == nil {
		return ""
	}

	var b strings.Builder
	b.WriteString(`<div class="tooltip">`)

	writeRow := func(label, value string, wrap bool) {
		wrapClass := ""
		if wrap {
			wrapClass = " wrap"
		}
		b.WriteString(fmt.Sprintf(`<div class="tooltip-row"><span class="tooltip-label">%s</span><span class="tooltip-value%s">%s</span></div>`,
			template.HTMLEscapeString(label),
			wrapClass,
			template.HTMLEscapeString(value)))
	}

	// Description
	if job.Description != "" {
		writeRow("Desc", job.Description, true)
	}

	// Tags
	if len(job.Tags) > 0 {
		writeRow("Tags", strings.Join(job.Tags, ", "), false)
	}

	// Directory
	dir := job.DisplayWorkingDir()
	if dir != "" {
		writeRow("Directory", dir, false)
	}

	// Command
	cmd := job.EffectiveCommand()
	writeRow("Command", cmd, true)

	// Timing
	if job.StartTime > 0 {
		t := time.Unix(job.StartTime, 0)
		writeRow("Started", t.Format("2006-01-02 15:04:05"), false)
	}
	if job.EndTime != nil {
		t := time.Unix(*job.EndTime, 0)
		writeRow("Ended", t.Format("2006-01-02 15:04:05"), false)
		if job.StartTime > 0 {
			duration := time.Duration(*job.EndTime-job.StartTime) * time.Second
			writeRow("Duration", formatDuration(duration), false)
		}
	} else if job.StartTime > 0 && (job.Status == db.StatusRunning || job.Status == db.StatusPaused) {
		duration := time.Since(time.Unix(job.StartTime, 0))
		writeRow("Running", formatDuration(duration), false)
	}

	// Exit code
	if job.ExitCode != nil {
		writeRow("Exit", fmt.Sprintf("%d", *job.ExitCode), false)
	}

	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// formatDuration formats a duration in a human-readable way
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	if hours < 24 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	days := hours / 24
	hours = hours % 24
	return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
}

// progressFetcher periodically fetches progress for running jobs
func (s *Server) progressFetcher() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			s.fetchAllProgress()
		}
	}
}

// fetchAllProgress fetches progress for all running jobs
func (s *Server) fetchAllProgress() {
	s.mu.RLock()
	jobs := append([]*db.Job(nil), s.jobs...)
	s.mu.RUnlock()

	// Collect running jobs
	var runningJobs []*db.Job
	for _, job := range jobs {
		if job != nil && job.Status == db.StatusRunning {
			runningJobs = append(runningJobs, job)
		}
	}

	if len(runningJobs) == 0 {
		return
	}

	// Fetch progress for each running job concurrently
	var wg sync.WaitGroup
	results := make(chan struct {
		jobID int64
		prog  *progress.Progress
	}, len(runningJobs))

	for _, job := range runningJobs {
		wg.Add(1)
		go func(j *db.Job) {
			defer wg.Done()
			prog := s.fetchJobProgress(j)
			if prog != nil {
				results <- struct {
					jobID int64
					prog  *progress.Progress
				}{j.ID, prog}
			}
		}(job)
	}

	// Close results channel when all fetches complete
	go func() {
		wg.Wait()
		close(results)
	}()

	// Collect results
	newProgress := make(map[int64]*progress.Progress)
	for result := range results {
		newProgress[result.jobID] = result.prog
	}

	// Update progress map
	s.mu.Lock()
	for jobID, prog := range newProgress {
		s.jobProgress[jobID] = prog
	}
	// Clean up progress for non-running jobs
	runningIDs := make(map[int64]bool)
	for _, job := range runningJobs {
		runningIDs[job.ID] = true
	}
	for jobID := range s.jobProgress {
		if !runningIDs[jobID] {
			delete(s.jobProgress, jobID)
		}
	}
	s.mu.Unlock()
}

// fetchJobProgress fetches progress for a single job by grepping its log file
func (s *Server) fetchJobProgress(job *db.Job) *progress.Progress {
	if job == nil {
		return nil
	}

	logFile, ok := logfiles.Resolve(job)
	if !ok || logFile == "" {
		return nil
	}

	// Quick grep for last progress line (case-insensitive)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	grepCmd := fmt.Sprintf("grep -i 'Progress:' %s 2>/dev/null | tail -1", logFile)
	stdout, _, err := ssh.RunWithContext(ctx, job.Host, grepCmd)
	if err != nil || strings.TrimSpace(stdout) == "" {
		return nil
	}

	return progress.ParseProgress(strings.TrimSpace(stdout))
}

// getJobProgress returns the progress for a job if available
func (s *Server) getJobProgress(jobID int64) *progress.Progress {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jobProgress[jobID]
}
