package tui

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/queuejob"
)

func (m Model) refreshJobs() tea.Cmd {
	return func() tea.Msg {
		jobs, err := db.ListJobs(m.database, "", "", 1000, nil, "")
		if err != nil {
			return jobsRefreshedMsg{err: err}
		}
		// Also fetch job dependencies for queue view
		deps, err := db.GetJobDependencyInfo(m.database)
		if err != nil {
			// Non-fatal, just ignore it
		}
		return jobsRefreshedMsg{jobs: jobs, jobDependencies: deps}
	}
}

func (m Model) restartJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		workingDir := job.WorkingDir
		command := job.Command
		description := job.Description

		if workingDir == "" || command == "" {
			return jobRestartedMsg{oldJobID: job.ID, err: fmt.Errorf("missing working directory or command")}
		}

		// Use ops package to restart job (queues if host offline)
		result, err := ops.RestartJob(database, ops.RestartJobParams{
			OriginalJob: job,
			WorkingDir:  workingDir,
			Command:     command,
			Description: description,
		}, ops.DefaultOptions())

		if err != nil {
			return jobRestartedMsg{oldJobID: job.ID, err: err}
		}

		return jobRestartedMsg{
			oldJobID: job.ID,
			newJobID: result.JobID,
			deferred: result.Deferred,
		}
	}
}

func (m Model) retryJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		if job.Host == "" {
			return jobRetriedMsg{oldJobID: job.ID, err: fmt.Errorf("job missing host")}
		}
		if job.Command == "" {
			return jobRetriedMsg{oldJobID: job.ID, err: fmt.Errorf("job missing command")}
		}
		workingDir := job.WorkingDir
		if workingDir == "" {
			if dir := job.EffectiveWorkingDir(); dir != "" {
				workingDir = dir
			} else {
				workingDir = "~"
			}
		}
		result, err := ops.QueueJob(database, ops.QueueJobParams{
			Host:        job.Host,
			WorkingDir:  workingDir,
			Command:     job.Command,
			Description: job.Description,
			EnvVars:     job.EnvVars,
		}, ops.DefaultOptions())
		if err != nil {
			return jobRetriedMsg{oldJobID: job.ID, err: err}
		}
		if err := rewireDependenciesForRetry(database, job.ID, result.JobID); err != nil {
			return jobRetriedMsg{oldJobID: job.ID, newJobID: result.JobID, err: err, deferred: result.Deferred}
		}
		return jobRetriedMsg{oldJobID: job.ID, newJobID: result.JobID, deferred: result.Deferred}
	}
}

func rewireDependenciesForRetry(database *sql.DB, oldID, newID int64) error {
	depJobs, err := db.ListQueuedJobsWithDependency(database, oldID)
	if err != nil {
		return err
	}
	for _, depJob := range depJobs {
		newSpec, changed := db.ReplaceDepSpecID(depJob.DepSpec, oldID, newID)
		if !changed {
			continue
		}
		if err := db.SetJobDepSpec(database, depJob.ID, newSpec); err != nil {
			return err
		}
		depJob.DepSpec = newSpec
		if _, err := ops.RequestQueueUpdate(database, depJob, ops.OptionsForMode(ops.TimeoutFast)); err != nil {
			return err
		}
	}
	return nil
}

// startQueuedJobNow starts a queued job immediately, bypassing any dependencies
func (m Model) startQueuedJobNow(job *db.Job) tea.Cmd {
	if job == nil {
		return func() tea.Msg {
			return jobStartedNowMsg{err: fmt.Errorf("no job selected")}
		}
	}
	// Use EffectiveStatus to be consistent with the handler
	if job.EffectiveStatus() != db.StatusQueued {
		return func() tea.Msg {
			return jobStartedNowMsg{jobID: job.ID, err: fmt.Errorf("job %d is %s, not queued", job.ID, job.EffectiveStatus())}
		}
	}
	database := m.database
	return func() tea.Msg {
		deferred, err := queuejob.StartNow(database, job)
		if err != nil {
			return jobStartedNowMsg{jobID: job.ID, host: job.Host, err: err}
		}
		return jobStartedNowMsg{jobID: job.ID, host: job.Host, deferred: deferred}
	}
}

// moveJobToFront moves a queued job to the front of its queue
func (m Model) moveJobToFront(job *db.Job) tea.Cmd {
	if job == nil || job.EffectiveStatus() != db.StatusQueued {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		result, err := ops.RequestQueuePriority(database, job, ops.OptionsForMode(ops.TimeoutFast))
		if err != nil {
			return jobMovedToFrontMsg{jobID: job.ID, host: job.Host, err: err}
		}
		return jobMovedToFrontMsg{jobID: job.ID, host: job.Host, moved: result.Moved, deferred: result.Deferred}
	}
}

// regenerateDescription generates an AI description for a job
func (m Model) regenerateDescription(job *db.Job) tea.Cmd {
	if job == nil || m.llmGenerator == nil {
		return nil
	}
	return func() tea.Msg {
		desc, hash, err := m.llmGenerator.GenerateOne(job)
		if err != nil {
			return descriptionGeneratedMsg{jobID: job.ID, err: err}
		}
		// Save to database
		if err := db.UpdateJobGeneratedDescription(m.database, job.ID, desc, hash); err != nil {
			return descriptionGeneratedMsg{jobID: job.ID, err: err}
		}
		return descriptionGeneratedMsg{jobID: job.ID, description: desc}
	}
}

func (m *Model) applyJobFilter() {
	prevSelectedID := int64(0)
	selectedIdx := m.jobList.Index()
	if len(m.jobs) > 0 && selectedIdx >= 0 && selectedIdx < len(m.jobs) {
		prevSelectedID = m.jobs[selectedIdx].ID
	}

	var filtered []*db.Job
	for _, job := range m.allJobs {
		if jobMatchesFilter(job, m.jobFilter) && m.jobMatchesHostFilter(job) {
			filtered = append(filtered, job)
		}
	}
	m.jobs = filtered

	// Apply sorting
	m.sortJobs()

	// Update the list items
	m.jobList.SetItems(JobsToListItems(m.jobs))

	// Try to restore selection to the same job
	if prevSelectedID != 0 {
		for i, job := range m.jobs {
			if job.ID == prevSelectedID {
				m.jobList.Select(i)
				break
			}
		}
	}

	if m.selectedJob != nil && (!jobMatchesFilter(m.selectedJob, m.jobFilter) || !m.jobMatchesHostFilter(m.selectedJob)) {
		m.detailTab = DetailTabDetails
		m.selectedJob = nil
		m.logContent = ""
		m.logStale = false
	}
}

// sortJobs sorts m.jobs in place according to m.jobSort
func (m *Model) sortJobs() {
	if m.jobFilter == jobFilterRecent {
		sortRecentJobs(m.jobs, m.jobSort)
		return
	}

	sortJobsByMode(m.jobs, m.jobSort)
}

// sortJobsByMode sorts jobs strictly according to the selected sort mode.
func sortJobsByMode(jobs []*db.Job, mode jobSortMode) {
	sort.Slice(jobs, func(i, j int) bool {
		return jobLessBySortMode(jobs[i], jobs[j], mode)
	})
}

// sortRecentJobs sorts jobs for Recent/Newest views:
// 1. Running jobs by start time
// 2. Queued jobs by queue order (order they will start)
// 3. Draft jobs
// 4. Terminal state jobs by completion time
func sortRecentJobs(jobs []*db.Job, mode jobSortMode) {
	sort.SliceStable(jobs, func(i, j int) bool {
		ji, jj := jobs[i], jobs[j]
		pi, pj := recentStatusPriority(ji), recentStatusPriority(jj)
		if pi != pj {
			return pi < pj
		}

		// Within same status group, apply appropriate ordering
		switch ji.EffectiveStatus() {
		case db.StatusRunning, db.StatusStarting, db.StatusPaused:
			// Running: most recently started first (for Newest), oldest first (for Oldest)
			if mode == jobSortOldest {
				return ji.StartTime < jj.StartTime
			}
			return ji.StartTime > jj.StartTime

		case db.StatusQueued:
			// Queued: by queue order (earliest queued = runs first, so show first)
			if ji.QueuedAt > 0 && jj.QueuedAt > 0 {
				return ji.QueuedAt < jj.QueuedAt
			}
			// Fallback to ID for legacy jobs
			return ji.ID < jj.ID

		case db.StatusDraft:
			// Draft: by creation time
			return jobLessBySortMode(ji, jj, mode)

		default:
			// Terminal states: by end time
			ti, tj := int64(0), int64(0)
			if ji.EndTime != nil {
				ti = *ji.EndTime
			}
			if jj.EndTime != nil {
				tj = *jj.EndTime
			}
			if ti != tj {
				if mode == jobSortOldest {
					return ti < tj
				}
				return ti > tj
			}
			// Fallback to standard sort
			return jobLessBySortMode(ji, jj, mode)
		}
	})
}

func recentStatusPriority(job *db.Job) int {
	switch job.EffectiveStatus() {
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return 0
	case db.StatusQueued, db.StatusPendingPlacement:
		return 1
	case db.StatusDraft:
		return 2
	default:
		return 3
	}
}

func jobLessBySortMode(i, j *db.Job, mode jobSortMode) bool {
	switch mode {
	case jobSortNewest:
		return getJobSortTime(i) > getJobSortTime(j)
	case jobSortOldest:
		return getJobSortTime(i) < getJobSortTime(j)
	case jobSortIDDesc:
		return i.ID > j.ID
	case jobSortIDAsc:
		return i.ID < j.ID
	case jobSortQueueOrder:
		return jobQueueOrderLess(i, j)
	default:
		return getJobSortTime(i) > getJobSortTime(j)
	}
}

// getJobSortTime returns the time to use for sorting (start time, created time, or ID)
func getJobSortTime(job *db.Job) int64 {
	if job.StartTime > 0 {
		return job.StartTime
	}
	// For jobs that never started, use created_at if available
	if job.CreatedAt > 0 {
		return job.CreatedAt
	}
	// For legacy jobs without created_at, use ID as a proxy (higher ID = more recent)
	return job.ID
}

// jobQueueOrderLess returns true if job i should come before job j in queue order
func jobQueueOrderLess(i, j *db.Job) bool {
	// Status priority: queued/draft first, then running/starting, then completed/dead/failed
	statusPriority := func(job *db.Job) int {
		switch job.EffectiveStatus() {
		case db.StatusQueued, db.StatusDraft:
			return 0
		case db.StatusRunning, db.StatusStarting, db.StatusPaused:
			return 1
		default:
			return 2
		}
	}

	pi, pj := statusPriority(i), statusPriority(j)
	if pi != pj {
		return pi < pj
	}

	// Within same status group:
	// - Queued/Draft: by queued_at (earlier = runs first), falling back to ID for legacy jobs
	// - Running: by start time (oldest first = running longest)
	// - Completed: by end time (most recent first)
	iStatus := i.EffectiveStatus()
	if iStatus == db.StatusQueued || iStatus == db.StatusDraft {
		// For queued/draft jobs, use queued_at for proper queue order
		if i.QueuedAt > 0 && j.QueuedAt > 0 {
			return i.QueuedAt < j.QueuedAt
		}
		// Fallback to ID for legacy jobs without queued_at
		return i.ID < j.ID
	} else if iStatus == db.StatusRunning || iStatus == db.StatusStarting {
		// Running jobs: show oldest (running longest) first
		return i.StartTime < j.StartTime
	} else {
		// Completed/failed/dead: most recently finished first
		ti, tj := int64(0), int64(0)
		if i.EndTime != nil {
			ti = *i.EndTime
		}
		if j.EndTime != nil {
			tj = *j.EndTime
		}
		return ti > tj
	}
}

func jobMatchesFilter(job *db.Job, mode jobFilterMode) bool {
	status := job.EffectiveStatus()

	switch mode {
	case jobFilterRecent:
		// Active jobs (running, starting, queued, draft, pending_placement)
		if status == db.StatusRunning || status == db.StatusStarting || status == db.StatusPaused || status == db.StatusQueued || status == db.StatusDraft || status == db.StatusPendingPlacement {
			return true
		}
		return isRecentHistory(job)
	case jobFilterActive:
		return status == db.StatusRunning || status == db.StatusStarting || status == db.StatusPaused || status == db.StatusQueued || status == db.StatusDraft || status == db.StatusPendingPlacement
	case jobFilterSucceeded:
		return status == db.StatusCompleted && job.ExitCode != nil && *job.ExitCode == 0
	case jobFilterFailed:
		if status == db.StatusFailed || status == db.StatusDead || status == db.StatusKilled || status == db.StatusCanceled {
			return true
		}
		return status == db.StatusCompleted && (job.ExitCode == nil || *job.ExitCode != 0)
	default:
		return true
	}
}

func (m Model) jobMatchesHostFilter(job *db.Job) bool {
	switch m.jobHostFilterMode {
	case hostFilterAll:
		return true
	case hostFilterSpecific:
		if m.jobHostFilterHost == "" {
			return true
		}
		return job.Host == m.jobHostFilterHost
	case hostFilterRecent:
		// Cloud-managed and unplaced jobs do not produce normal host sync timestamps,
		// but they should remain visible in the default Jobs view.
		if job.Host == "" || job.IsCloudJob() || db.IsCloudHost(job.Host) {
			return true
		}
		return m.isHostRecentlySynced(job.Host)
	default:
		return true
	}
}

func isRecentHistory(job *db.Job) bool {
	const window = 24 * time.Hour
	windowSeconds := int64(window / time.Second)
	now := time.Now()
	if job.EndTime != nil {
		return now.Unix()-*job.EndTime < windowSeconds
	}
	if job.Status == db.StatusDead || job.Status == db.StatusFailed || job.Status == db.StatusKilled || job.Status == db.StatusCanceled {
		timestamp := job.StartTime
		if timestamp == 0 {
			timestamp = job.CreatedAt
		}
		if timestamp == 0 {
			return false
		}
		return now.Sub(time.Unix(timestamp, 0)) < window
	}
	return false
}

func jobFilterDescription(mode jobFilterMode) string {
	switch mode {
	case jobFilterRecent:
		return "Recent"
	case jobFilterActive:
		return "Active"
	case jobFilterSucceeded:
		return "Succeeded"
	case jobFilterFailed:
		return "Failed"
	default:
		return "All"
	}
}

func hostFilterDescription(m Model) string {
	switch m.jobHostFilterMode {
	case hostFilterRecent:
		return "Synced <2d"
	case hostFilterAll:
		return "All"
	case hostFilterSpecific:
		if m.jobHostFilterHost != "" {
			return m.jobHostFilterHost
		}
		return "Host"
	default:
		return "All"
	}
}

func (m Model) getTargetJob() *db.Job {
	if !m.jobSelectionActive {
		return nil
	}
	if m.detailTab == DetailTabLogs && m.selectedJob != nil {
		return m.selectedJob
	}
	idx := m.jobList.Index()
	if len(m.jobs) > 0 && idx >= 0 && idx < len(m.jobs) {
		return m.jobs[idx]
	}
	return nil
}

func (m Model) killJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}

	jobID := job.ID
	cancelled := job.EffectiveStatus() == db.StatusQueued
	return func() tea.Msg {
		result, err := m.coreService.KillJob(jobID, ops.TimeoutFast)
		if err != nil {
			return jobKilledMsg{jobID: jobID, err: err, cancelled: cancelled}
		}
		return jobKilledMsg{
			jobID:     jobID,
			deferred:  result.Outcome.Deferred,
			cancelled: cancelled,
			message:   result.Outcome.Message,
		}
	}
}

func (m Model) draftJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}
	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.DraftJob(jobID, ops.TimeoutFast)
		if err != nil {
			return jobDraftedMsg{jobID: jobID, err: err}
		}
		return jobDraftedMsg{
			jobID:    jobID,
			deferred: result.Outcome.Deferred,
			message:  result.Outcome.Message,
		}
	}
}

func (m Model) queueDraftJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}
	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.RequestStatus(jobID, db.StatusQueued, ops.TimeoutFast)
		if err != nil {
			return jobQueuedMsg{jobID: jobID, err: err}
		}
		return jobQueuedMsg{
			jobID:    jobID,
			deferred: result.Outcome.Deferred,
			message:  result.Outcome.Message,
		}
	}
}

func (m Model) runDraftJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}
	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.RequestStatus(jobID, db.StatusRunning, ops.TimeoutFast)
		if err != nil {
			return jobQueuedMsg{jobID: jobID, err: err}
		}
		return jobQueuedMsg{
			jobID:    jobID,
			deferred: result.Outcome.Deferred,
			message:  result.Outcome.Message,
		}
	}
}

func (m Model) cancelQueuedJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}

	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.KillJob(jobID, ops.TimeoutFast)
		if err != nil {
			return jobKilledMsg{jobID: jobID, err: err, cancelled: true}
		}
		return jobKilledMsg{
			jobID:     jobID,
			deferred:  result.Outcome.Deferred,
			cancelled: true,
			message:   result.Outcome.Message,
		}
	}
}

func (m Model) pauseJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}
	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.PauseJob(jobID, ops.TimeoutFast)
		if err != nil {
			return jobPausedMsg{jobID: jobID, err: err}
		}
		return jobPausedMsg{
			jobID:    jobID,
			deferred: result.Outcome.Deferred,
			message:  result.Outcome.Message,
		}
	}
}

func (m Model) resumeJob(job *db.Job) tea.Cmd {
	if job == nil || m.coreService == nil {
		return nil
	}
	jobID := job.ID
	return func() tea.Msg {
		result, err := m.coreService.ResumeJob(jobID, ops.TimeoutFast)
		if err != nil {
			return jobResumedMsg{jobID: jobID, err: err}
		}
		return jobResumedMsg{
			jobID:    jobID,
			deferred: result.Outcome.Deferred,
			message:  result.Outcome.Message,
		}
	}
}

func (m Model) pruneJobs() tea.Cmd {
	return func() tea.Msg {
		count, err := db.PruneJobs(m.database, false, nil)
		return pruneCompletedMsg{count: count, err: err}
	}
}

func (m Model) removeJob(job *db.Job) tea.Cmd {
	if job == nil {
		return nil
	}
	database := m.database
	return func() tea.Msg {
		err := db.TombstoneJob(database, job.ID)
		return jobRemovedMsg{jobID: job.ID, err: err}
	}
}
