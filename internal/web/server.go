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

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/hostinfo"
	"github.com/osteele/remote-jobs/internal/monitor"
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
	}, nil
}

func (s *Server) Start() (string, error) {
	if s.monitor == nil {
		return "", fmt.Errorf("monitor is required")
	}
	s.captureSnapshot()
	go s.consumeEvents()

	listener, err := net.Listen("tcp", s.addr)
	if err != nil {
		return "", err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	s.server = &http.Server{
		Handler: mux,
	}

	go func() {
		_ = s.server.Serve(listener)
	}()

	return "http://" + listener.Addr().String(), nil
}

func (s *Server) Stop(ctx context.Context) error {
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
}

type jobRow struct {
	ID          int64
	Host        string
	Status      string
	Time        string
	GPU         string
	Description string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	jobs := append([]*db.Job(nil), s.jobs...)
	hosts := append([]*hostinfo.Host(nil), s.hosts...)
	hostSyncTimes := make(map[string]time.Time, len(s.hostSyncTimes))
	for key, value := range s.hostSyncTimes {
		hostSyncTimes[key] = value
	}
	lastUpdated := s.lastUpdated
	s.mu.RUnlock()

	selectedView := normalizeView(r.URL.Query().Get("view"))
	selectedHost := normalizeHostFilter(r.URL.Query().Get("host"))

	filteredJobs := filterJobsByView(jobs, selectedView)
	filteredJobs = filterJobsByHost(filteredJobs, selectedHost, hostSyncTimes)
	sortJobsForView(filteredJobs, selectedView)

	showGPU := false
	rows := make([]jobRow, 0, len(filteredJobs))
	for _, job := range filteredJobs {
		gpu := job.GetGPU()
		if gpu != "" {
			showGPU = true
		}
		if gpu == "" {
			gpu = "—"
		}
		rows = append(rows, jobRow{
			ID:          job.ID,
			Host:        job.Host,
			Status:      formatJobStatus(job),
			Time:        formatJobTime(job),
			GPU:         gpu,
			Description: job.EffectiveDescription(),
		})
	}

	hostFilters := buildHostFilters(hosts)
	hostSummaries := buildHostSummaries(hosts)

	refreshSeconds := refreshIntervalSeconds(jobs, s.monitor)
	data := pageData{
		Title:          "remote-jobs",
		Views:          viewOptions,
		SelectedView:   selectedView,
		HostFilters:    hostFilters,
		SelectedHost:   selectedHost,
		HostSummaries:  hostSummaries,
		Jobs:           rows,
		ShowGPU:        showGPU,
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

func buildHostSummaries(hosts []*hostinfo.Host) []hostSummary {
	summaries := make([]hostSummary, 0, len(hosts))
	for _, host := range hosts {
		if host == nil {
			continue
		}
		statusClass := hostStatusClass(host.Status)
		cpuText := "--"
		ramText := "--"
		if pct, ok := hostinfo.HostCPULoadPercent(host); ok {
			cpuText = fmt.Sprintf("%d%%", pct)
		}
		if pct, ok := hostinfo.HostMemUsagePercent(host); ok {
			ramText = fmt.Sprintf("%d%%", pct)
		}
		summaries = append(summaries, hostSummary{
			Name:   host.Name,
			Status: statusLabel(host.Status),
			CPU:    cpuText,
			RAM:    ramText,
			Style:  statusClass,
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
