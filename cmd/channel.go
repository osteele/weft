package cmd

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/app/dbwatch"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/status"
	"github.com/osteele/weft/internal/ui/terminal"
	"github.com/osteele/weft/internal/watchevents"
	"github.com/spf13/cobra"
)

var channelCmd = &cobra.Command{
	Use:   "channel",
	Short: "Serve Claude Code channels",
}

var channelServeCmd = &cobra.Command{
	Use:   "serve",
	Short: "Serve Weft job events as a Claude Code channel",
	Args:  cobra.NoArgs,
	RunE:  runChannelServe,
}

var channelInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Weft as an MCP channel server",
	Long: `Install Weft as an MCP channel server.

The claude target updates ~/.claude.json. The json target prints a generic
MCP server entry for other agents.`,
	Args: cobra.NoArgs,
	RunE: runChannelInstall,
}

var (
	channelProject        string
	channelDebounce       time.Duration
	channelIncludeRunning bool

	channelInstallTarget     string
	channelInstallServerName string
	channelInstallCommand    string
	channelInstallDryRun     bool
)

func init() {
	rootCmd.AddCommand(channelCmd)
	channelCmd.AddCommand(channelServeCmd)
	channelCmd.AddCommand(channelInstallCmd)
	channelServeCmd.Flags().StringVar(&channelProject, "project", "", "Project name (default: repo root name for the working directory)")
	channelServeCmd.Flags().DurationVar(&channelDebounce, "debounce", 2*time.Second, "Per-job debounce window")
	channelServeCmd.Flags().BoolVar(&channelIncludeRunning, "include-running", false, "Emit running and starting transitions")
	channelInstallCmd.Flags().StringVar(&channelInstallTarget, "target", "claude", "Install target: claude or json")
	channelInstallCmd.Flags().StringVar(&channelInstallServerName, "server-name", "weft", "MCP server name")
	channelInstallCmd.Flags().StringVar(&channelInstallCommand, "command", "weft", "Command used by the MCP client")
	channelInstallCmd.Flags().BoolVar(&channelInstallDryRun, "dry-run", false, "Print the config change without writing files")
}

func runChannelServe(cmd *cobra.Command, _ []string) error {
	project, err := workdirProjectName(channelProject)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "weft channel: %v\n", err)
		return err
	}

	database, err := db.OpenForReading()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	if err := validateChannelProject(database, project); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "weft channel: %v\n", err)
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "weft channel: project=%s\n", project)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	server := newChannelServer(database, project, channelDebounce, channelIncludeRunning, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	return server.serve(ctx)
}

func workdirProjectName(explicit string) (string, error) {
	var args []string
	if strings.TrimSpace(explicit) != "" {
		args = []string{explicit}
	}
	return resolveProjectArg(args)
}

func runChannelInstall(cmd *cobra.Command, _ []string) error {
	entry := channelMCPServerEntry(channelInstallCommand)
	switch channelInstallTarget {
	case "json":
		return printChannelInstallJSON(cmd.OutOrStdout(), channelInstallServerName, entry)
	case "claude":
		return installChannelForClaude(cmd.OutOrStdout(), channelInstallServerName, entry, channelInstallDryRun)
	default:
		return usageErrorf("unknown --target %q (supported: claude, json)", channelInstallTarget)
	}
}

func channelMCPServerEntry(command string) map[string]any {
	return map[string]any{
		"command": command,
		"args":    []string{"channel", "serve"},
	}
}

func printChannelInstallJSON(w io.Writer, serverName string, entry map[string]any) error {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			serverName: entry,
		},
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(cfg)
}

func installChannelForClaude(w io.Writer, serverName string, entry map[string]any, dryRun bool) error {
	path, err := claudeConfigPath()
	if err != nil {
		return err
	}
	cfg, err := readClaudeConfig(path)
	if err != nil {
		return err
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		servers = map[string]any{}
		cfg["mcpServers"] = servers
	}
	servers[serverName] = entry
	if dryRun {
		return writeIndentedJSON(w, cfg)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(w, "Installed MCP server %q in %s\n", serverName, path)
	fmt.Fprintf(w, "Start Claude Code with: claude --dangerously-load-development-channels server:%s\n", serverName)
	return nil
}

func claudeConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return home + string(os.PathSeparator) + ".claude.json", nil
}

func readClaudeConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg == nil {
		cfg = map[string]any{}
	}
	return cfg, nil
}

func writeIndentedJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func validateChannelProject(database *sql.DB, project string) error {
	hasAny, err := db.ProjectHasAnyJobs(database, project)
	if err != nil {
		return err
	}
	if !hasAny {
		return fmt.Errorf("no jobs found for project %q; is the current directory a known project?", project)
	}
	return nil
}

type channelServer struct {
	database       *sql.DB
	project        string
	debounce       time.Duration
	includeRunning bool
	in             io.Reader
	out            io.Writer
	errOut         io.Writer
	writeMu        sync.Mutex
}

func newChannelServer(database *sql.DB, project string, debounce time.Duration, includeRunning bool, in io.Reader, out io.Writer, errOut io.Writer) *channelServer {
	return &channelServer{
		database:       database,
		project:        project,
		debounce:       debounce,
		includeRunning: includeRunning,
		in:             in,
		out:            out,
		errOut:         errOut,
	}
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (s *channelServer) serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	started := make(chan struct{})
	var startOnce sync.Once
	go func() {
		<-started
		if err := s.runNotifications(ctx); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(s.errOut, "weft channel: %v\n", err)
		}
		cancel()
	}()

	scanner := bufio.NewScanner(s.in)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg rpcMessage
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			fmt.Fprintf(s.errOut, "weft channel: invalid JSON-RPC message: %v\n", err)
			continue
		}
		switch msg.Method {
		case "initialize":
			if err := s.writeResponse(msg.ID, map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities": map[string]any{
					"experimental": map[string]any{
						"claude/channel": map[string]any{},
					},
				},
				"serverInfo": map[string]string{
					"name":    "weft",
					"version": Version,
				},
				"instructions": `One-way Weft job lifecycle alerts arrive as <channel source="weft" project="..." job_id="...">. Read them as project-scoped job updates and use the included Weft commands when follow-up is needed.`,
			}); err != nil {
				cancel()
				return err
			}
		case "notifications/initialized", "initialized":
			startOnce.Do(func() { close(started) })
		default:
			if len(msg.ID) != 0 {
				if err := s.writeError(msg.ID, -32601, "method not found"); err != nil {
					cancel()
					return err
				}
			}
		}
	}
	cancel()
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func (s *channelServer) runNotifications(ctx context.Context) error {
	if err := s.emitStartupSummary(); err != nil {
		return err
	}
	return s.watchProject(ctx)
}

func (s *channelServer) writeResponse(id json.RawMessage, result any) error {
	return s.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	})
}

func (s *channelServer) writeError(id json.RawMessage, code int, message string) error {
	return s.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func (s *channelServer) writeChannel(content string, meta map[string]string) error {
	return s.writeJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/claude/channel",
		"params": map[string]any{
			"content": content,
			"meta":    meta,
		},
	})
}

func (s *channelServer) writeJSON(v any) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	enc := json.NewEncoder(s.out)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (s *channelServer) emitStartupSummary() error {
	counts, err := loadUnprocessedCounts(s.database, s.project)
	if err != nil {
		return fmt.Errorf("count unprocessed jobs: %w", err)
	}
	content := formatChannelStartupSummary(s.project, counts.Completed, counts.Failed)
	return s.writeChannel(content, map[string]string{
		"project":                 s.project,
		"event_type":              "startup_summary",
		"unprocessed_completed":   strconv.Itoa(counts.Completed),
		"unprocessed_failed":      strconv.Itoa(counts.Failed),
		"unprocessed_window_days": "14",
	})
}

func formatChannelStartupSummary(project string, completed, failed int) string {
	if completed == 0 && failed == 0 {
		return fmt.Sprintf("%s has no unprocessed terminal jobs from the last 14 days.", project)
	}
	total := completed + failed
	return fmt.Sprintf("%s has %d unprocessed terminal %s from the last 14 days: %d completed, %d failed. Review: weft jobs list --project %s --unprocessed --group-by status",
		project, total, pluralize("job", total), completed, failed, project)
}

func (s *channelServer) watchProject(ctx context.Context) error {
	tracker := watchevents.NewTransitionTracker()
	launchTracker := newLaunchTransitionTracker()
	debouncer := newChannelDebouncer(s.debounce, func(ev channelEvent) {
		if err := s.writeChannel(ev.Content, ev.Meta); err != nil {
			fmt.Fprintf(s.errOut, "weft channel: send notification: %v\n", err)
		}
	})
	defer debouncer.Stop()

	source, err := dbwatch.OpenChangeSource()
	if err != nil {
		fmt.Fprintf(s.errOut, "weft channel: DB watcher unavailable: %v\n", err)
	}
	if source != nil {
		defer source.Close()
	}

	for {
		jobs, launches, err := loadChannelProjectSnapshot(s.database, s.project)
		if err != nil {
			return err
		}
		now := time.Now()
		for _, ev := range tracker.Diff(jobs, now) {
			if !s.shouldEmitJobTransition(ev) {
				continue
			}
			debouncer.Submit(channelDebounceKey{kind: "job", id: ev.ID}, channelEventForJob(ev))
		}
		for _, ev := range launchTracker.Diff(launches) {
			if !shouldEmitLaunchTransition(ev) {
				continue
			}
			debouncer.Submit(channelDebounceKey{kind: "launch", id: ev.ID}, channelEventForLaunch(s.project, ev))
		}

		if _, err := source.Wait(ctx, terminal.TerminalSyncInterval); err != nil {
			if ctx.Err() != nil {
				return context.Canceled
			}
			fmt.Fprintf(s.errOut, "weft channel: DB watcher wait failed: %v\n", err)
			if channelWaitOrDone(ctx, terminal.TerminalSyncInterval) {
				return context.Canceled
			}
		}
	}
}

func (s *channelServer) shouldEmitJobTransition(ev watchevents.TransitionEvent) bool {
	if ev.IsTerminal() {
		return true
	}
	if !s.includeRunning {
		return false
	}
	return ev.Status == status.Running || ev.Status == status.Starting
}

func loadChannelProjectSnapshot(database *sql.DB, project string) ([]*db.Job, []*db.Launch, error) {
	groups, err := terminal.LoadProjectWatchGroups(database, 24*time.Hour)
	if err != nil {
		return nil, nil, err
	}
	groups = terminal.FilterProjectGroups(groups, project)
	if len(groups) == 0 {
		if err := validateChannelProject(database, project); err != nil {
			return nil, nil, err
		}
	}
	return flattenChannelProjectJobs(groups), flattenChannelProjectLaunches(groups), nil
}

func flattenChannelProjectJobs(groups []terminal.ProjectGroup) []*db.Job {
	all := make([][]*db.Job, 0, 4*len(groups))
	for _, g := range groups {
		all = append(all, g.Running, g.Queued, g.Unplaced, g.Recent)
	}
	return watchevents.DedupeJobsByID(all...)
}

func flattenChannelProjectLaunches(groups []terminal.ProjectGroup) []*db.Launch {
	seen := make(map[int64]struct{})
	var launches []*db.Launch
	for _, g := range groups {
		for _, launch := range g.CloudInsts {
			if launch == nil {
				continue
			}
			if _, ok := seen[launch.ID]; ok {
				continue
			}
			seen[launch.ID] = struct{}{}
			launches = append(launches, launch)
		}
	}
	return launches
}

func channelEventForJob(ev watchevents.TransitionEvent) channelEvent {
	meta := map[string]string{
		"project": ev.Project,
		"job_id":  ev.JobID,
		"status":  ev.Status,
	}
	if ev.Host != "" {
		meta["host"] = ev.Host
	}
	if ev.ExitCode != nil {
		meta["exit_code"] = strconv.Itoa(*ev.ExitCode)
	}
	if ev.InstanceID != nil {
		meta["cloud_instance_id"] = ids.FormatInstanceID(*ev.InstanceID)
	}

	summary := ev.JobID
	if ev.Project != "" {
		summary = ev.Project + "/" + ev.JobID
	}
	host := ""
	if ev.Host != "" {
		host = " on " + ev.Host
	}
	exit := ""
	if ev.ExitCode != nil {
		exit = fmt.Sprintf(" (exit %d)", *ev.ExitCode)
	}
	return channelEvent{
		Content: fmt.Sprintf("%s %s%s%s. Logs: weft jobs logs %s", summary, ev.Status, host, exit, ev.JobID),
		Meta:    meta,
	}
}

type launchTransitionEvent struct {
	ID             int64
	Status         string
	PrevStatus     string
	CampaignID     *int64
	Termination    string
	Provider       string
	ProviderInstID string
}

type launchTransitionTracker struct {
	prev   map[int64]string
	seeded bool
}

func newLaunchTransitionTracker() *launchTransitionTracker {
	return &launchTransitionTracker{prev: make(map[int64]string)}
}

func (t *launchTransitionTracker) Diff(launches []*db.Launch) []launchTransitionEvent {
	cur := make(map[int64]*db.Launch, len(launches))
	for _, launch := range launches {
		if launch != nil {
			cur[launch.ID] = launch
		}
	}
	if !t.seeded {
		for id, launch := range cur {
			t.prev[id] = launch.Status
		}
		t.seeded = true
		return nil
	}
	var events []launchTransitionEvent
	for id, launch := range cur {
		prev, hadPrev := t.prev[id]
		if hadPrev && prev == launch.Status {
			continue
		}
		events = append(events, launchTransitionEvent{
			ID:             id,
			Status:         launch.Status,
			PrevStatus:     prev,
			CampaignID:     launch.CampaignID,
			Termination:    launch.TerminationReason,
			Provider:       launch.Provider,
			ProviderInstID: launch.EffectiveProviderID(),
		})
		t.prev[id] = launch.Status
	}
	for id := range t.prev {
		if _, ok := cur[id]; !ok {
			delete(t.prev, id)
		}
	}
	return events
}

func shouldEmitLaunchTransition(ev launchTransitionEvent) bool {
	switch ev.Status {
	case db.LaunchStatusGrace, db.LaunchStatusCompleted, db.LaunchStatusFailed, db.LaunchStatusCancelled:
		return true
	default:
		return false
	}
}

func channelEventForLaunch(project string, ev launchTransitionEvent) channelEvent {
	instanceID := ids.FormatInstanceID(ev.ID)
	meta := map[string]string{
		"project":           project,
		"cloud_instance_id": instanceID,
		"status":            ev.Status,
	}
	if ev.CampaignID != nil {
		meta["campaign_id"] = strconv.FormatInt(*ev.CampaignID, 10)
	}
	if ev.Termination != "" {
		meta["termination_reason"] = ev.Termination
	}
	if ev.Provider != "" {
		meta["provider"] = ev.Provider
	}
	if ev.ProviderInstID != "" {
		meta["provider_instance_id"] = ev.ProviderInstID
	}
	return channelEvent{
		Content: fmt.Sprintf("%s cloud instance %s is %s. Diagnose: weft instance diagnose %s", project, instanceID, ev.Status, instanceID),
		Meta:    meta,
	}
}

type channelEvent struct {
	Content string
	Meta    map[string]string
}

type channelDebounceKey struct {
	kind string
	id   int64
}

type channelDebouncer struct {
	delay time.Duration
	emit  func(channelEvent)
	mu    sync.Mutex
	items map[channelDebounceKey]*channelDebounceItem
}

type channelDebounceItem struct {
	timer *time.Timer
	event channelEvent
}

func newChannelDebouncer(delay time.Duration, emit func(channelEvent)) *channelDebouncer {
	return &channelDebouncer{
		delay: delay,
		emit:  emit,
		items: make(map[channelDebounceKey]*channelDebounceItem),
	}
}

func (d *channelDebouncer) Submit(key channelDebounceKey, ev channelEvent) {
	if d.delay <= 0 {
		d.emit(ev)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if item, ok := d.items[key]; ok {
		item.event = ev
		item.timer.Reset(d.delay)
		return
	}
	item := &channelDebounceItem{event: ev}
	item.timer = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		latest := item.event
		delete(d.items, key)
		d.mu.Unlock()
		d.emit(latest)
	})
	d.items[key] = item
}

func (d *channelDebouncer) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, item := range d.items {
		item.timer.Stop()
		delete(d.items, key)
	}
}

func channelWaitOrDone(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-t.C:
		return false
	}
}

func pluralize(noun string, n int) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}
