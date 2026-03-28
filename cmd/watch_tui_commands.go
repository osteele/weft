package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/tui"
)

// ---------------------------------------------------------------------------
// Retry helpers
// ---------------------------------------------------------------------------

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

func (m watchModel) hasRetryableFailures() bool {
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if ok && u.Launch != nil && db.IsRetryableTermination(u.Launch) {
			return true
		}
	}
	return len(m.partialErrors) > 0 && !m.partialErrorsRetried
}

func (m watchModel) countFailedInstances() int {
	count := 0
	for _, id := range m.instanceIDs {
		u, ok := m.updates[id]
		if ok && u.Launch != nil && u.Launch.Status == db.LaunchStatusFailed {
			count++
		}
	}
	if len(m.partialErrorJobs) > 0 {
		count++
	}
	return count
}

// ---------------------------------------------------------------------------
// On-prem sync helpers
// ---------------------------------------------------------------------------

func (m watchModel) requestOnPremSyncs() {
	if m.syncWorker == nil {
		return
	}
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

// ---------------------------------------------------------------------------
// Job preservation
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Refresh commands
// ---------------------------------------------------------------------------

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

func refreshWatchSystem(database *sql.DB, cfg *config.Config) tea.Cmd {
	return func() tea.Msg {
		snapshot, err := loadWatchSystemSnapshot(database, cfg, nil, false)
		return watchAllRefreshedMsg{snapshot: snapshot, err: err}
	}
}

func requestWatchJobUnplace(database *sql.DB, jobID int64) tea.Cmd {
	return func() tea.Msg {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("get job %d: %w", jobID, err)}
		}
		if job == nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("job %d not found", jobID)}
		}
		result, err := ops.UnplaceQueuedJob(database, job, ops.OptionsForMode(ops.TimeoutFast))
		if err != nil {
			return watchUnplaceDoneMsg{err: err}
		}
		updatedJob, err := db.GetJobByID(database, jobID)
		if err != nil {
			return watchUnplaceDoneMsg{err: fmt.Errorf("reload job %d: %w", jobID, err)}
		}
		return watchUnplaceDoneMsg{job: updatedJob, message: result.Message}
	}
}

func requestWatchJobSubmit(ctx context.Context, database *sql.DB, r2Client *r2.Client, jobID, instanceID int64) tea.Cmd {
	return func() tea.Msg {
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return watchSubmitDoneMsg{jobID: jobID, instanceID: instanceID, err: fmt.Errorf("get job %d: %w", jobID, err)}
		}
		if job == nil {
			return watchSubmitDoneMsg{jobID: jobID, instanceID: instanceID, err: fmt.Errorf("job %d not found", jobID)}
		}
		if err := campaign.SubmitJobsToInstance(ctx, database, r2Client, instanceID, []*db.Job{job}); err != nil {
			return watchSubmitDoneMsg{jobID: jobID, instanceID: instanceID, err: err}
		}
		return watchSubmitDoneMsg{jobID: jobID, instanceID: instanceID}
	}
}

func refreshWatchOnPrem(database *sql.DB) tea.Cmd {
	return func() tea.Msg {
		onPremJobs, err := db.ListActiveOnPremJobs(database)
		if err != nil {
			return watchOnPremRefreshedMsg{err: err}
		}
		unplacedJobs, err := db.ListUnplacedJobs(database)
		if err != nil {
			return watchOnPremRefreshedMsg{err: err}
		}
		return watchOnPremRefreshedMsg{
			onPremHosts:  groupOnPremHosts(onPremJobs),
			unplacedJobs: unplacedJobs,
		}
	}
}

// ---------------------------------------------------------------------------
// Tick scheduling
// ---------------------------------------------------------------------------

func scheduleSyncTick() tea.Cmd {
	return tea.Tick(15*time.Second, func(time.Time) tea.Msg {
		return watchSyncTickMsg{}
	})
}

func scheduleCheckDone() tea.Cmd {
	return tea.Tick(5*time.Second, func(time.Time) tea.Msg {
		return watchCheckDoneMsg{}
	})
}

func scheduleWatchAllTick() tea.Cmd {
	return tea.Tick(15*time.Second, func(time.Time) tea.Msg {
		return watchAllTickMsg{}
	})
}
