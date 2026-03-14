package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
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
)

// initialInstanceInfo holds pre-fetched DB data for instances that haven't
// received a channel update yet, avoiding repeated queries in View().
type initialInstanceInfo struct {
	ci       *db.CloudInstance
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
	cloudInstances map[int64]*db.CloudInstance
	jobs           map[int64][]*db.Job
	outcomes       map[int64]map[int64]string // instanceID → (jobID → outcome)
	quitAfter      bool
}

// watchCheckDoneMsg triggers a periodic DB-based check for all-terminal state.
type watchCheckDoneMsg struct{}

// watchCheckDoneResultMsg carries the result of a background DB terminal check.
type watchCheckDoneResultMsg struct{ allTerminal bool }

func newWatchModel(database *sql.DB, instanceIDs []int64, r2Client *r2.Client) watchModel {
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
		ci, _ := db.GetCloudInstance(database, id)
		jobs, _ := db.GetCloudInstanceJobsIncludingAttempts(database, id)
		outcomes, _ := db.GetAttemptOutcomesByInstance(database, id)
		initInfo[id] = initialInstanceInfo{ci: ci, jobs: jobs, outcomes: outcomes}
	}

	campaignID, launchedAt := campaignInfoFromInstances(database, instanceIDs)

	return watchModel{
		instanceIDs:    instanceIDs,
		updates:        make(map[int64]campaign.InstanceUpdate),
		channels:       make(map[int64]<-chan campaign.InstanceUpdate),
		initInfo:       initInfo,
		database:       database,
		clients:        clients,
		r2Client:       r2Client,
		spinner:        s,
		ctx:            ctx,
		cancel:         cancel,
		campaignID:     campaignID,
		launchedAt:     launchedAt,
		jobProgressHWM: make(map[int64]int),
		reconciler:     campaign.NewReconciler(),
	}
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
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.cancel()
			return m, tea.Quit
		}

	case watchUpdateMsg:
		if msg.closed {
			clearWatchJobProgressHWM(m.jobProgressHWM, m.updates[msg.instanceID])
			// Channel closed — instance reached terminal state
			return m, m.checkAllDone()
		}
		prev := m.updates[msg.instanceID]
		if msg.update.CloudInstance != nil && campaign.IsInstanceTerminal(msg.update.CloudInstance.Status) {
			msg.update.Jobs = preserveWatchCurrentJobs(msg.update.CloudInstance.ID, prev.Jobs, msg.update.Jobs)
		}
		updateWatchJobProgressHWM(m.jobProgressHWM, prev, msg.update)
		m.updates[msg.instanceID] = msg.update
		// Continue reading from the same channel
		ch := m.channels[msg.instanceID]
		return m, waitForUpdate(msg.instanceID, ch)

	case watchSyncTickMsg:
		// Reconcile cloud instances and sync job results without blocking the UI
		return m, tea.Batch(
			func() tea.Msg {
				cfg, _ := config.Load()
				clients := buildCloudClients(cfg)
				r2Client, _ := buildR2Client(cfg)
				result := syncCloudStateWithClients(cfg, m.database, m.reconciler, clients, r2Client, false)
				if result.ReconcileResult != nil && len(result.ReconcileResult.TerminatedInstances) > 0 {
					// Trigger relaunch in background
					go func() {
						relaunchCfg := campaign.RelaunchConfig{
							Clients:    clients,
							R2Cfg:      cfg.Vastai.R2.ToCloudR2Config(),
							CreateOpts: cloud.CreateOpts{},
							LaunchOpts: campaign.LaunchOpts{GracePeriodSeconds: 15 * 60},
							Database:   m.database,
						}
						if rr, err := campaign.RelaunchOrphanedJobs(relaunchCfg); err != nil {
							log.Printf("relaunch: %v", err)
						} else if rr != nil && len(rr.InstanceIDs) > 0 {
							log.Printf("relaunch: launched %d new instances", len(rr.InstanceIDs))
						}
					}()
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
			if ok && u.CloudInstance != nil && campaign.IsInstanceTerminal(u.CloudInstance.Status) {
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
			u.CloudInstance = ci
			m.updates[id] = u
		}
		for id, jobs := range msg.jobs {
			u := m.updates[id]
			if u.CloudInstance != nil && campaign.IsInstanceTerminal(u.CloudInstance.Status) {
				u.Jobs = preserveWatchCurrentJobs(u.CloudInstance.ID, u.Jobs, jobs)
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
				ci, err := db.GetCloudInstance(m.database, id)
				if err != nil || ci == nil || !campaign.IsInstanceTerminal(ci.Status) {
					return watchCheckDoneResultMsg{allTerminal: false}
				}
			}
			return watchCheckDoneResultMsg{allTerminal: true}
		}

	case watchCheckDoneResultMsg:
		if msg.allTerminal {
			return m, refreshWatchInstancesFromDB(m.database, m.instanceIDs, true)
		}
		return m, scheduleCheckDone()

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
		if job == nil || job.CloudInstanceID == nil || *job.CloudInstanceID != instanceID {
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
		if job.CloudInstanceID != nil && *job.CloudInstanceID == instanceID {
			preserved = append(preserved, job)
			continue
		}

		jobCopy := *job
		preservedInstanceID := instanceID
		jobCopy.CloudInstanceID = &preservedInstanceID
		preserved = append(preserved, &jobCopy)
	}
	return preserved
}

func (m watchModel) checkAllDone() tea.Cmd {
	return func() tea.Msg {
		for _, id := range m.instanceIDs {
			ci, err := db.GetCloudInstance(m.database, id)
			if err != nil || ci == nil || !campaign.IsInstanceTerminal(ci.Status) {
				return watchCheckDoneResultMsg{allTerminal: false}
			}
		}
		return watchCheckDoneResultMsg{allTerminal: true}
	}
}

func refreshWatchInstancesFromDB(database *sql.DB, instanceIDs []int64, quitAfter bool) tea.Cmd {
	return func() tea.Msg {
		cloudInstances := make(map[int64]*db.CloudInstance, len(instanceIDs))
		jobs := make(map[int64][]*db.Job, len(instanceIDs))
		outcomes := make(map[int64]map[int64]string, len(instanceIDs))
		for _, id := range instanceIDs {
			if ci, err := db.GetCloudInstance(database, id); err == nil && ci != nil {
				cloudInstances[id] = ci
			}
			if instanceJobs, err := db.GetCloudInstanceJobsIncludingAttempts(database, id); err == nil && instanceJobs != nil {
				jobs[id] = instanceJobs
			}
			if instanceOutcomes, err := db.GetAttemptOutcomesByInstance(database, id); err == nil {
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

func (m watchModel) View() string {
	var b strings.Builder

	// Campaign header
	if m.campaignID > 0 {
		header := fmt.Sprintf("Campaign %d", m.campaignID)
		if !m.launchedAt.IsZero() {
			header += fmt.Sprintf(" — launched %s (%s ago)",
				m.launchedAt.Format("15:04"),
				time.Since(m.launchedAt).Truncate(time.Second))
		}
		b.WriteString(watchTitleStyle.Render(header))
		b.WriteString("\n\n")
	}

	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if !ok {
			// No channel update yet — show pre-fetched DB data
			info := m.initInfo[id]
			ci := info.ci
			jobs := info.jobs
			if ci != nil {
				update := campaign.InstanceUpdate{
					CloudInstance:      ci,
					Jobs:               jobs,
					JobAttemptOutcomes: info.outcomes,
				}
				resolved := 0
				b.WriteString(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{
					spinner:                  m.spinner.View(),
					showSpinnerIfNonTerminal: true,
					resolvedJobsOverride:     &resolved,
					dimJobStatuses:           true,
				}))
				b.WriteString("\n\n")
			} else {
				b.WriteString(m.spinner.View())
				b.WriteString(fmt.Sprintf(" Instance %d — waiting for data...\n\n", id))
			}
			continue
		}

		b.WriteString(formatWatchInstanceBlock(u, m.jobProgressHWM, watchInstanceBlockOptions{}))
		b.WriteString("\n\n")
	}

	// Campaign cost total
	if len(m.instanceIDs) > 1 {
		var totalCost float64
		hasCost := false
		for _, id := range m.instanceIDs {
			u, ok := m.updates[id]
			if !ok || u.CloudInstance == nil || u.Instance == nil || u.CloudInstance.LaunchedAt == nil {
				continue
			}
			uptime := time.Since(time.Unix(*u.CloudInstance.LaunchedAt, 0)).Truncate(time.Second)
			totalCost += uptime.Hours() * u.Instance.CostPerHour
			hasCost = true
		}
		if hasCost {
			b.WriteString(watchTitleStyle.Render(fmt.Sprintf("Total: $%.2f (%d instances)", totalCost, len(m.instanceIDs))))
			b.WriteString("\n\n")
		}
	}

	if !m.done {
		b.WriteString(watchDimStyle.Render("q to quit (instances continue in background)"))
		b.WriteString("\n")
	}

	return b.String()
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
func watchInstances(database *sql.DB, instanceIDs []int64) error {
	cfg, _ := config.Load()
	r2Client, _ := buildR2Client(cfg)
	model := newWatchModel(database, instanceIDs, r2Client)

	origLogOutput := log.Writer()
	log.SetOutput(io.Discard)
	defer log.SetOutput(origLogOutput)

	p := tea.NewProgram(model, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return err
	}

	if finalView := renderWatchExitSnapshot(finalModel); finalView != "" {
		fmt.Print(finalView)
	}
	return nil
}

func renderWatchExitSnapshot(model tea.Model) string {
	m, ok := model.(watchModel)
	if !ok || !m.done {
		return ""
	}
	return m.View()
}
