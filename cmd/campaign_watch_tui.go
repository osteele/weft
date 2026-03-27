package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/tui"
)

// initialInstanceInfo holds pre-fetched DB data for instances that haven't
// received a channel update yet, avoiding repeated queries in View().
type initialInstanceInfo struct {
	ci       *db.Launch
	jobs     []*db.Job
	outcomes map[int64]string
}

type watchModel struct {
	instanceIDs    []int64
	updates        map[int64]campaign.InstanceUpdate // latest update per instance
	channels       map[int64]<-chan campaign.InstanceUpdate
	initInfo       map[int64]initialInstanceInfo // pre-fetched data for pre-update display
	database       *sql.DB
	clients        map[int64]cloud.Client // per-instance client (looked up from DB provider)
	r2Client       *r2.Client             // R2 client for phase/bootstrap fetching (may be nil)
	spinner        spinner.Model
	done           bool
	err            error
	ctx            context.Context
	cancel         context.CancelFunc
	campaignID     int64         // campaign ID (0 if unknown)
	launchedAt     time.Time     // campaign launch time
	jobProgressHWM map[int64]int // high-water mark per job ID (prevents progress regression)
	reconciler     *campaign.Reconciler
	syncWorker     *tui.SyncWorker

	// Retry state
	appConfig            *config.Config // config for building cloud clients on retry
	retrying             bool           // true while retry launch is in progress
	retryResult          string         // status line after retry completes (or error)
	retryAttempt         int            // current retry attempt number (0-based)
	retryExtraAttempts   int            // extra attempts allowed for this retry round
	partialErrors        []string       // human-readable launch failure messages (inline watch only)
	partialErrorJobs     []*db.Job      // jobs from launch failures (inline watch only)
	partialErrorsRetried bool           // true after partial error jobs have been retried

	// Donor relationship cache (recomputed when instanceIDs change)
	cachedHiddenIDs   map[int64]bool
	cachedDonorChains map[int64][]*db.Launch
	donorCacheDirty   bool // true when instanceIDs or donor info has changed

	// Viewport scrolling
	height    int // terminal height from WindowSizeMsg
	scrollOff int // lines scrolled up from bottom (0 = pinned to bottom)
}

// Styles for the watch TUI (allocated once, not per-render).
var (
	watchTitleStyle     = lipgloss.NewStyle().Bold(true)
	watchStatusStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	watchRunningStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	watchCompletedStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	watchFailedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	watchDimStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
)

type watchUpdateMsg struct {
	instanceID int64
	update     campaign.InstanceUpdate
	closed     bool // true if channel was closed
}

// watchSyncTickMsg triggers periodic cloud job result syncing.
type watchSyncTickMsg struct{}

// watchSyncDoneMsg is sent after syncCloudJobResults completes,
// triggering a background re-read of jobs from DB for terminated instances.
type watchSyncDoneMsg struct{}

// watchJobsRefreshedMsg carries refreshed job lists from a background DB query.
type watchJobsRefreshedMsg struct {
	cloudInstances map[int64]*db.Launch
	jobs           map[int64][]*db.Job
	outcomes       map[int64]map[int64]string // instanceID → (jobID → outcome)
	quitAfter      bool
}

// watchCheckDoneMsg triggers a periodic DB-based check for all-terminal state.
type watchCheckDoneMsg struct{}

// watchCheckDoneResultMsg carries the result of a background DB terminal check.
type watchCheckDoneResultMsg struct{ allTerminal bool }

// campaignWatchSyncResultMsg is sent when the background SyncWorker produces a result.
type campaignWatchSyncResultMsg struct{}

// retryResultMsg carries the result of retrying failed instances.
type retryResultMsg struct {
	instanceIDs []int64
	skipped     int // jobs that exceeded max attempts
	err         error
}

// retryBackoffMsg triggers a delayed retry attempt after no offers were found.
type retryBackoffMsg struct{}

// retryBackoffDelays defines the delay before each retry attempt (indexed by attempt number).
// The length of this slice determines the maximum number of retry attempts.
var retryBackoffDelays = []time.Duration{
	30 * time.Second,
	60 * time.Second,
	2 * time.Minute,
	5 * time.Minute,
}

func newWatchModel(database *sql.DB, instanceIDs []int64, r2Client *r2.Client, cfg *config.Config) watchModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	ctx, cancel := context.WithCancel(context.Background())

	// Build per-instance clients from DB provider field
	clients := make(map[int64]cloud.Client)
	for _, id := range instanceIDs {
		clients[id] = clientForInstance(database, id)
	}

	// Pre-fetch DB data for initial display (avoids queries in View)
	initInfo := make(map[int64]initialInstanceInfo)
	for _, id := range instanceIDs {
		ci, _ := db.GetLaunch(database, id)
		jobs, _ := db.GetLaunchJobsIncludingAttempts(database, id)
		outcomes, _ := db.GetAttemptOutcomesByLaunch(database, id)
		initInfo[id] = initialInstanceInfo{ci: ci, jobs: jobs, outcomes: outcomes}
	}

	campaignID, launchedAt := campaignInfoFromInstances(database, instanceIDs)

	sw := tui.NewSyncWorker(database, nil, nil, cfg)
	sw.Start()
	go func() { <-ctx.Done(); sw.Stop() }()

	m := watchModel{
		instanceIDs:     instanceIDs,
		updates:         make(map[int64]campaign.InstanceUpdate),
		channels:        make(map[int64]<-chan campaign.InstanceUpdate),
		initInfo:        initInfo,
		database:        database,
		clients:         clients,
		r2Client:        r2Client,
		spinner:         s,
		ctx:             ctx,
		cancel:          cancel,
		campaignID:      campaignID,
		launchedAt:      launchedAt,
		jobProgressHWM:  make(map[int64]int),
		reconciler:      campaign.NewReconciler(),
		syncWorker:      sw,
		appConfig:       cfg,
		donorCacheDirty: true,
	}
	m.rebuildDonorCache()
	return m
}

func (m watchModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	cmds = append(cmds, m.spinner.Tick)

	for _, id := range m.instanceIDs {
		client := m.clients[id]
		ch := campaign.WatchInstance(m.ctx, client, m.database, id, 2*time.Second, 10*time.Second, m.r2Client)
		m.channels[id] = ch
		cmds = append(cmds, waitForUpdate(id, ch))
	}

	// Start periodic cloud job sync and DB-based done check
	cmds = append(cmds, scheduleSyncTick())
	cmds = append(cmds, scheduleCheckDone())

	// Start on-prem syncs and arm the result drainer
	m.requestOnPremSyncs()
	cmds = append(cmds, m.syncWorker.WaitForResult(m.ctx, func(tui.SyncResult) tea.Msg {
		return campaignWatchSyncResultMsg{}
	}))

	return tea.Batch(cmds...)
}

// waitForUpdate reads the next value from an instance watch channel.
func waitForUpdate(instanceID int64, ch <-chan campaign.InstanceUpdate) tea.Cmd {
	return func() tea.Msg {
		update, ok := <-ch
		return watchUpdateMsg{instanceID: instanceID, update: update, closed: !ok}
	}
}

func (m watchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.cancel()
			return m, tea.Quit
		case "r":
			if !m.retrying && m.hasRetryableFailures() {
				m.retryAttempt = 0
				m.retrying = true
				m.retryResult = ""
				m.retryExtraAttempts = campaign.DefaultMaxCloudAttempts
				return m, m.retryFailedInstances(m.retryExtraAttempts)
			}
		case "up", "k":
			m.scrollOff++
			return m, nil
		case "down", "j":
			if m.scrollOff > 0 {
				m.scrollOff--
			}
			return m, nil
		case "G":
			m.scrollOff = 0
			return m, nil
		case "g":
			m.scrollOff = math.MaxInt
			return m, nil
		}

	case watchUpdateMsg:
		if msg.closed {
			clearWatchJobProgressHWM(m.jobProgressHWM, m.updates[msg.instanceID])
			// Channel closed — instance reached terminal state
			return m, m.checkAllDone()
		}
		prev := m.updates[msg.instanceID]
		if msg.update.Launch != nil && campaign.IsInstanceTerminal(msg.update.Launch.Status) {
			msg.update.Jobs = preserveWatchCurrentJobs(msg.update.Launch.ID, prev.Jobs, msg.update.Jobs)
		}
		updateWatchJobProgressHWM(m.jobProgressHWM, prev, msg.update)
		m.updates[msg.instanceID] = msg.update

		// Auto-relaunch on retryable infrastructure failure (preemption, infra failure, failed to launch)
		ci := msg.update.Launch
		ch := m.channels[msg.instanceID]
		if ci != nil && db.IsRetryableTermination(ci) && !m.retrying && m.database != nil {
			m.retrying = true
			m.retryResult = ""
			return m, tea.Batch(
				waitForUpdate(msg.instanceID, ch),
				m.retryFailedInstances(0),
			)
		}

		// Continue reading from the same channel
		return m, waitForUpdate(msg.instanceID, ch)

	case watchSyncTickMsg:
		// Sync on-prem hosts alongside cloud instances
		m.requestOnPremSyncs()
		// Lightweight DB-only reconciliation — WatchInstance goroutines handle cloud API calls
		return m, tea.Batch(
			func() tea.Msg {
				if _, err := db.ResetJobsOnTerminalLaunches(m.database); err != nil {
					log.Printf("reset jobs on terminal instances: %v", err)
				}
				if _, err := campaign.ReconcileCampaigns(m.database); err != nil {
					log.Printf("reconcile campaigns: %v", err)
				}
				return watchSyncDoneMsg{}
			},
			scheduleSyncTick(),
		)

	case watchSyncDoneMsg:
		// Re-read jobs from DB for terminated instances in a background goroutine
		// to avoid blocking the UI thread with DB queries.
		terminalIDs := make([]int64, 0)
		for _, id := range m.instanceIDs {
			u, ok := m.updates[id]
			if ok && u.Launch != nil && campaign.IsInstanceTerminal(u.Launch.Status) {
				terminalIDs = append(terminalIDs, id)
			}
		}
		if len(terminalIDs) == 0 {
			return m, nil
		}
		return m, refreshWatchInstancesFromDB(m.database, terminalIDs, false)

	case watchJobsRefreshedMsg:
		for id, ci := range msg.cloudInstances {
			u := m.updates[id]
			u.Launch = ci
			m.updates[id] = u
		}
		for id, jobs := range msg.jobs {
			u := m.updates[id]
			if u.Launch != nil && campaign.IsInstanceTerminal(u.Launch.Status) {
				u.Jobs = preserveWatchCurrentJobs(u.Launch.ID, u.Jobs, jobs)
			} else {
				u.Jobs = jobs
			}
			if outcomes, ok := msg.outcomes[id]; ok {
				u.JobAttemptOutcomes = outcomes
			}
			m.updates[id] = u
		}
		if msg.quitAfter {
			m.done = true
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Tick(2*time.Second, func(time.Time) tea.Msg {
				return tea.QuitMsg{}
			})
		}
		return m, nil

	case watchCheckDoneMsg:
		if m.done {
			return m, nil
		}
		// Run DB queries in background to avoid blocking the UI
		return m, func() tea.Msg {
			for _, id := range m.instanceIDs {
				ci, err := db.GetLaunch(m.database, id)
				if err != nil || ci == nil || !campaign.IsInstanceTerminal(ci.Status) {
					return watchCheckDoneResultMsg{allTerminal: false}
				}
			}
			return watchCheckDoneResultMsg{allTerminal: true}
		}

	case watchCheckDoneResultMsg:
		if msg.allTerminal {
			if m.retrying {
				// Retry in progress — don't quit yet, recheck later
				return m, scheduleCheckDone()
			}
			return m, refreshWatchInstancesFromDB(m.database, m.instanceIDs, true)
		}
		return m, scheduleCheckDone()

	case retryResultMsg:
		m.retrying = false
		if msg.err != nil {
			m.retryResult = fmt.Sprintf("Retry failed: %v", msg.err)
			return m, m.checkAllDone()
		}
		if len(msg.instanceIDs) == 0 {
			// All remaining jobs exceeded max retry attempts — stop retrying
			if msg.skipped > 0 {
				m.retryExtraAttempts = 0
				m.retryResult = fmt.Sprintf("Retry: %d job(s) exceeded max cloud attempts, giving up", msg.skipped)
				return m, m.checkAllDone()
			}
			if m.retryAttempt < len(retryBackoffDelays) {
				delay := retryBackoffDelays[m.retryAttempt]
				m.retryAttempt++
				m.retryResult = fmt.Sprintf("Retry: no offers available, retrying in %s (attempt %d/%d)",
					delay, m.retryAttempt+1, len(retryBackoffDelays)+1)
				return m, tea.Tick(delay, func(time.Time) tea.Msg { return retryBackoffMsg{} })
			}
			m.retryExtraAttempts = 0
			m.retryResult = fmt.Sprintf("Retry: no instances launched after %d attempts (no offers available)", len(retryBackoffDelays)+1)
			return m, m.checkAllDone()
		}
		// Success — reset retry state
		m.retryAttempt = 0
		m.retryExtraAttempts = 0
		// Add new instances to the watch view
		var cmds []tea.Cmd
		for _, id := range msg.instanceIDs {
			m.instanceIDs = append(m.instanceIDs, id)
			m.clients[id] = clientForInstance(m.database, id)
			// Pre-fetch initial info
			ci, _ := db.GetLaunch(m.database, id)
			jobs, _ := db.GetLaunchJobsIncludingAttempts(m.database, id)
			outcomes, _ := db.GetAttemptOutcomesByLaunch(m.database, id)
			m.initInfo[id] = initialInstanceInfo{ci: ci, jobs: jobs, outcomes: outcomes}
			// Start watching
			client := m.clients[id]
			ch := campaign.WatchInstance(m.ctx, client, m.database, id, 2*time.Second, 10*time.Second, m.r2Client)
			m.channels[id] = ch
			cmds = append(cmds, waitForUpdate(id, ch))
		}
		// Mark partial errors as retried (clears the banner in inline watch)
		m.partialErrorJobs = nil
		m.partialErrorsRetried = true
		// Rebuild donor cache since new instances may reference failed predecessors
		m.rebuildDonorCache()
		m.scrollOff = 0 // snap to bottom to show new instances
		return m, tea.Batch(cmds...)

	case retryBackoffMsg:
		m.retrying = true
		m.retryResult = ""
		return m, m.retryFailedInstances(m.retryExtraAttempts)

	case campaignWatchSyncResultMsg:
		// Background on-prem sync completed — re-arm and continue (no display update needed)
		return m, m.syncWorker.WaitForResult(m.ctx, func(tui.SyncResult) tea.Msg {
			return campaignWatchSyncResultMsg{}
		})

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}

	return m, nil
}

func preserveWatchCurrentJobs(instanceID int64, prevJobs, jobs []*db.Job) []*db.Job {
	if instanceID == 0 || len(jobs) == 0 {
		return jobs
	}

	currentJobIDs := make(map[int64]struct{})
	for _, job := range prevJobs {
		if job == nil || job.LaunchID == nil || *job.LaunchID != instanceID {
			continue
		}
		currentJobIDs[job.ID] = struct{}{}
	}
	if len(currentJobIDs) == 0 {
		return jobs
	}

	preserved := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if job == nil {
			preserved = append(preserved, nil)
			continue
		}
		if _, ok := currentJobIDs[job.ID]; !ok {
			preserved = append(preserved, job)
			continue
		}
		if job.LaunchID != nil && *job.LaunchID == instanceID {
			preserved = append(preserved, job)
			continue
		}

		jobCopy := *job
		preservedInstanceID := instanceID
		jobCopy.LaunchID = &preservedInstanceID
		preserved = append(preserved, &jobCopy)
	}
	return preserved
}

func (m watchModel) checkAllDone() tea.Cmd {
	return func() tea.Msg {
		for _, id := range m.instanceIDs {
			ci, err := db.GetLaunch(m.database, id)
			if err != nil || ci == nil || !campaign.IsInstanceTerminal(ci.Status) {
				return watchCheckDoneResultMsg{allTerminal: false}
			}
		}
		return watchCheckDoneResultMsg{allTerminal: true}
	}
}

// retryableJobs collects unplaced jobs from failed instances in this watch session,
// plus any jobs from partial launch failures (inline watch only).
func (m watchModel) retryableJobs() []*db.Job {
	seen := make(map[int64]struct{})
	var jobs []*db.Job

	// Jobs from failed instances that are now unplaced
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if !ok {
			continue
		}
		if u.Launch == nil || u.Launch.Status != db.LaunchStatusFailed {
			continue
		}
		// Get unplaced jobs that were on this instance
		instanceJobs, err := db.GetLaunchJobsIncludingAttempts(m.database, id)
		if err != nil {
			continue
		}
		for _, j := range instanceJobs {
			if j == nil {
				continue
			}
			if _, ok := seen[j.ID]; ok {
				continue
			}
			// Only include jobs that are queued and unplaced (ready for retry)
			fresh, err := db.GetJobByID(m.database, j.ID)
			if err != nil || fresh == nil {
				continue
			}
			if fresh.Status == db.StatusQueued && (fresh.Host == "" || fresh.LaunchID == nil) {
				seen[fresh.ID] = struct{}{}
				jobs = append(jobs, fresh)
			}
		}
	}

	// Jobs from partial launch failures (never got an instance)
	for _, j := range m.partialErrorJobs {
		if j == nil {
			continue
		}
		if _, ok := seen[j.ID]; ok {
			continue
		}
		fresh, err := db.GetJobByID(m.database, j.ID)
		if err != nil || fresh == nil {
			continue
		}
		if fresh.Status == db.StatusQueued {
			seen[fresh.ID] = struct{}{}
			jobs = append(jobs, fresh)
		}
	}

	return jobs
}

// retryFailedInstances resets orphaned jobs on terminal instances and launches
// new cloud instances for them. extraAttempts raises the max attempt threshold
// (used for manual retries). Runs in a background goroutine.
func (m watchModel) retryFailedInstances(extraAttempts int) tea.Cmd {
	database := m.database
	cfg := m.appConfig
	return func() tea.Msg {
		result, err := attemptRelaunchOrphanedJobs(database, cfg, extraAttempts)
		if err != nil {
			return retryResultMsg{err: err}
		}
		if result == nil {
			return retryResultMsg{}
		}
		return retryResultMsg{instanceIDs: result.InstanceIDs, skipped: result.Skipped}
	}
}

// hasRetryableFailures reports whether any instances have retryable infrastructure failures.
func (m watchModel) hasRetryableFailures() bool {
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if ok && u.Launch != nil && db.IsRetryableTermination(u.Launch) {
			return true
		}
	}
	return len(m.partialErrors) > 0 && !m.partialErrorsRetried
}

// countFailedInstances returns the number of failed instances in the watch view.
func (m watchModel) countFailedInstances() int {
	count := 0
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if ok && u.Launch != nil && u.Launch.Status == db.LaunchStatusFailed {
			count++
		}
	}
	if len(m.partialErrorJobs) > 0 {
		count++ // count partial errors as one logical failure group
	}
	return count
}

func refreshWatchInstancesFromDB(database *sql.DB, instanceIDs []int64, quitAfter bool) tea.Cmd {
	return func() tea.Msg {
		cloudInstances := make(map[int64]*db.Launch, len(instanceIDs))
		jobs := make(map[int64][]*db.Job, len(instanceIDs))
		outcomes := make(map[int64]map[int64]string, len(instanceIDs))
		for _, id := range instanceIDs {
			if ci, err := db.GetLaunch(database, id); err == nil && ci != nil {
				cloudInstances[id] = ci
			}
			if instanceJobs, err := db.GetLaunchJobsIncludingAttempts(database, id); err == nil && instanceJobs != nil {
				jobs[id] = instanceJobs
			}
			if instanceOutcomes, err := db.GetAttemptOutcomesByLaunch(database, id); err == nil {
				outcomes[id] = instanceOutcomes
			}
		}
		return watchJobsRefreshedMsg{
			cloudInstances: cloudInstances,
			jobs:           jobs,
			outcomes:       outcomes,
			quitAfter:      quitAfter,
		}
	}
}

func (m watchModel) campaignViews() []cloudInstanceView {
	views := make([]cloudInstanceView, 0, len(m.instanceIDs))
	for _, id := range m.instanceIDs {
		if update, ok := m.updates[id]; ok {
			views = append(views, cloudInstanceView{
				Launch:   update.Launch,
				Instance: update.Instance,
			})
			continue
		}
		if info, ok := m.initInfo[id]; ok {
			views = append(views, cloudInstanceView{Launch: info.ci})
		}
	}
	return views
}

func formatCampaignWatchSummaryLine(launchedAt time.Time, views []cloudInstanceView, now time.Time) string {
	agg := summarizeLaunches(views, now)
	label := "Summary:"
	if !launchedAt.IsZero() {
		label = "Summary: uptime: " + now.Sub(launchedAt).Truncate(time.Second).String()
	}
	return formatCloudAggregateSummary(label, agg)
}

// rebuildDonorCache recomputes which instances are superseded and their
// predecessor chains. Called from Update() when instance list changes.
func (m *watchModel) rebuildDonorCache() {
	hiddenIDs := make(map[int64]bool)
	donorChains := make(map[int64][]*db.Launch)

	getCI := func(id int64) *db.Launch {
		if u, ok := m.updates[id]; ok && u.Launch != nil {
			return u.Launch
		}
		if info, ok := m.initInfo[id]; ok && info.ci != nil {
			return info.ci
		}
		ci, _ := db.GetLaunch(m.database, id)
		return ci
	}

	for _, id := range m.instanceIDs {
		ci := getCI(id)
		if ci == nil || ci.DonorInstanceID == nil {
			continue
		}
		chain := collectDonorChain(ci, getCI)
		if len(chain) > 0 {
			donorChains[id] = chain
			for _, donor := range chain {
				hiddenIDs[donor.ID] = true
			}
		}
	}

	m.cachedHiddenIDs = hiddenIDs
	m.cachedDonorChains = donorChains
	m.donorCacheDirty = false
}

func (m watchModel) View() string {
	var b strings.Builder
	now := time.Now()

	// Campaign header
	if m.campaignID > 0 {
		header := fmt.Sprintf("Campaign %d", m.campaignID)
		if !m.launchedAt.IsZero() {
			header += fmt.Sprintf(" — launched %s (%s ago)",
				m.launchedAt.Format("15:04"),
				now.Sub(m.launchedAt).Truncate(time.Second))
		}
		b.WriteString(watchTitleStyle.Render(header))
		b.WriteString("\n\n")
	}
	if summary := formatCampaignWatchSummaryLine(m.launchedAt, m.campaignViews(), now); summary != "" {
		b.WriteString(summary)
		b.WriteString("\n\n")
	}

	for _, id := range m.instanceIDs {
		if m.cachedHiddenIDs[id] {
			continue
		}
		donors := m.cachedDonorChains[id]

		u, ok := m.updates[id]
		if !ok {
			// No channel update yet — show pre-fetched DB data
			info := m.initInfo[id]
			ci := info.ci
			jobs := info.jobs
			if ci != nil {
				update := campaign.InstanceUpdate{
					Launch:             ci,
					Jobs:               jobs,
					JobAttemptOutcomes: info.outcomes,
				}
				resolved := 0
				b.WriteString(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{
					spinner:                  m.spinner.View(),
					showSpinnerIfNonTerminal: true,
					resolvedJobsOverride:     &resolved,
					dimJobStatuses:           true,
					donorInstances:           donors,
				}))
				b.WriteString("\n\n")
			} else {
				b.WriteString(m.spinner.View())
				b.WriteString(fmt.Sprintf(" Instance %d — waiting for data...\n\n", id))
			}
			continue
		}

		b.WriteString(formatWatchInstanceBlock(u, m.jobProgressHWM, watchInstanceBlockOptions{
			donorInstances: donors,
		}))
		b.WriteString("\n\n")
	}

	// Partial launch errors (shown between instance blocks and retry status)
	if len(m.partialErrors) > 0 && !m.partialErrorsRetried {
		b.WriteString(formatPartialErrors(m.partialErrors))
		b.WriteString("\n")
	}

	// Retry status: show spinner while retrying, or error/failure messages.
	// Success results are conveyed by the "Previous:" line on the replacement block.
	if m.retrying {
		b.WriteString(m.spinner.View())
		b.WriteString(fmt.Sprintf(" Retrying %d failed instance(s)...\n", m.countFailedInstances()))
	} else if m.retryResult != "" {
		b.WriteString(m.retryResult)
		b.WriteString("\n")
	}

	if !m.done {
		hint := "j/k scroll  g/G top/bottom  q quit (instances continue in background)"
		if !m.retrying && m.hasRetryableFailures() {
			hint = "j/k scroll  g/G top/bottom  r retry  q quit (instances continue in background)"
		}
		b.WriteString(watchDimStyle.Render(hint))
		b.WriteString("\n")
	}

	return m.applyViewport(b.String())
}

// applyViewport slices rendered content to fit the terminal height.
// Bottom-anchored: scrollOff=0 shows the bottom of the content.
func (m watchModel) applyViewport(content string) string {
	if m.height <= 0 || m.done {
		return content
	}

	// Fast path: count newlines to check fit without allocating a []string
	if strings.Count(content, "\n") < m.height {
		return content
	}

	lines := strings.Split(content, "\n")
	// strings.Split produces a trailing empty element for content ending in \n
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) <= m.height {
		return content
	}

	maxOff := len(lines) - m.height
	off := m.scrollOff
	if off > maxOff {
		off = maxOff
	}

	end := len(lines) - off
	start := end - m.height
	if start < 0 {
		start = 0
	}

	visible := lines[start:end]

	if start > 0 {
		visible[0] = watchDimStyle.Render(fmt.Sprintf("↑ %d more lines above", start))
	}
	if off > 0 {
		visible[len(visible)-1] = watchDimStyle.Render(fmt.Sprintf("↓ %d more lines below", off))
	}

	return strings.Join(visible, "\n")
}

// requestOnPremSyncs requests background syncs for on-prem hosts with active jobs.
func (m watchModel) requestOnPremSyncs() {
	jobs, err := db.ListActiveOnPremJobs(m.database)
	if err != nil {
		return
	}
	byHost := make(map[string][]*db.Job)
	for _, job := range jobs {
		if job != nil && job.Host != "" {
			byHost[job.Host] = append(byHost[job.Host], job)
		}
	}
	for host, hostJobs := range byHost {
		m.syncWorker.Request(tui.SyncRequest{
			Host: host,
			Rate: tui.GetHostSyncRate(hostJobs),
		})
	}
}

// scheduleSyncTick returns a command that fires a sync tick after 15 seconds.
func scheduleSyncTick() tea.Cmd {
	return tea.Tick(15*time.Second, func(time.Time) tea.Msg {
		return watchSyncTickMsg{}
	})
}

// scheduleCheckDone returns a command that fires a done-check after 5 seconds.
func scheduleCheckDone() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg {
		return watchCheckDoneMsg{}
	})
}

// watchInstances runs the interactive TUI watch for one or more cloud instances.
// Returns the final list of instance IDs (which may include auto-relaunched instances).
func watchInstances(database *sql.DB, instanceIDs []int64) ([]int64, error) {
	cfg, _ := config.Load()
	r2Client, _ := buildR2Client(cfg)
	model := newWatchModel(database, instanceIDs, r2Client, cfg)

	origLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLogOutput)

	p := tea.NewProgram(model, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return instanceIDs, err
	}

	if finalView := renderWatchExitSnapshot(finalModel); finalView != "" {
		fmt.Print(finalView)
	}

	// Extract final instance IDs (may include auto-relaunched instances)
	if m, ok := finalModel.(watchModel); ok {
		return m.instanceIDs, nil
	}
	return instanceIDs, nil
}

func renderWatchExitSnapshot(model tea.Model) string {
	m, ok := model.(watchModel)
	if !ok || !m.done {
		return ""
	}
	return m.View()
}
