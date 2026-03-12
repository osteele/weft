package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
)

type onPremHostSummary struct {
	Name string
	Jobs []*db.Job
}

type watchSystemSnapshot struct {
	CloudInstances  []*db.CloudInstance
	InstanceUpdates map[int64]campaign.InstanceUpdate
	OnPremHosts     []onPremHostSummary
	UnplacedJobs    []*db.Job
}

func watchAllPlain(database *sql.DB, cfg *config.Config, follow bool) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	for {
		snapshot, err := loadWatchSystemSnapshot(database, cfg, true)
		if err != nil {
			return err
		}

		fmt.Print(formatWatchPlainSnapshot(snapshot, time.Now()))

		if !follow && snapshot.IsEmpty() {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(30 * time.Second):
		}
	}
}

func loadWatchSystemSnapshot(database *sql.DB, cfg *config.Config, refresh bool) (watchSystemSnapshot, error) {
	if refresh {
		performFastSync(database, false)
		clients := buildCloudClients(cfg)
		r2Client, _ := buildR2Client(cfg)
		if len(clients) > 0 {
			_, _ = campaign.NewReconciler().ReconcileCloudInstances(database, clients, r2Client)
		}
		syncCloudJobResults(cfg, database, false)
		_, _ = campaign.ReconcileCampaigns(database)
	}

	cloudInstances, err := db.ListRunningCloudInstances(database)
	if err != nil {
		return watchSystemSnapshot{}, fmt.Errorf("list running cloud instances: %w", err)
	}

	instanceUpdates := make(map[int64]campaign.InstanceUpdate, len(cloudInstances))
	for _, ci := range cloudInstances {
		jobs, err := db.GetCloudInstanceJobsIncludingAttempts(database, ci.ID)
		if err != nil {
			return watchSystemSnapshot{}, fmt.Errorf("list jobs for cloud instance %d: %w", ci.ID, err)
		}
		outcomes, err := db.GetAttemptOutcomesByInstance(database, ci.ID)
		if err != nil {
			return watchSystemSnapshot{}, fmt.Errorf("list attempt outcomes for cloud instance %d: %w", ci.ID, err)
		}
		instanceUpdates[ci.ID] = campaign.InstanceUpdate{
			CloudInstance:      ci,
			Jobs:               jobs,
			JobAttemptOutcomes: outcomes,
		}
	}

	onPremJobs, err := db.ListActiveOnPremJobs(database)
	if err != nil {
		return watchSystemSnapshot{}, fmt.Errorf("list active on-prem jobs: %w", err)
	}

	unplacedJobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return watchSystemSnapshot{}, fmt.Errorf("list unplaced jobs: %w", err)
	}

	return watchSystemSnapshot{
		CloudInstances:  cloudInstances,
		InstanceUpdates: instanceUpdates,
		OnPremHosts:     groupOnPremHosts(onPremJobs),
		UnplacedJobs:    unplacedJobs,
	}, nil
}

func (s watchSystemSnapshot) IsEmpty() bool {
	return len(s.CloudInstances) == 0 && len(s.OnPremHosts) == 0 && len(s.UnplacedJobs) == 0
}

func groupOnPremHosts(jobs []*db.Job) []onPremHostSummary {
	grouped := make(map[string][]*db.Job)
	for _, job := range jobs {
		if job == nil || strings.TrimSpace(job.Host) == "" {
			continue
		}
		grouped[job.Host] = append(grouped[job.Host], job)
	}

	names := make([]string, 0, len(grouped))
	for host := range grouped {
		names = append(names, host)
	}
	sort.Strings(names)

	summaries := make([]onPremHostSummary, 0, len(names))
	for _, host := range names {
		hostJobs := grouped[host]
		sort.SliceStable(hostJobs, func(i, j int) bool {
			pi := activeJobPriority(hostJobs[i])
			pj := activeJobPriority(hostJobs[j])
			if pi != pj {
				return pi < pj
			}
			return hostJobs[i].ID < hostJobs[j].ID
		})
		summaries = append(summaries, onPremHostSummary{Name: host, Jobs: hostJobs})
	}
	return summaries
}

func activeJobPriority(job *db.Job) int {
	switch job.EffectiveStatus() {
	case db.StatusRunning, db.StatusStarting, db.StatusPaused:
		return 0
	case db.StatusQueued:
		return 1
	default:
		return 2
	}
}

func formatWatchPlainSnapshot(snapshot watchSystemSnapshot, now time.Time) string {
	var b strings.Builder

	b.WriteString(fmt.Sprintf("=== System Watch %s ===\n\n", now.Format("2006-01-02 15:04:05")))

	b.WriteString(fmt.Sprintf("CLOUD INSTANCES (%d)\n", len(snapshot.CloudInstances)))
	if len(snapshot.CloudInstances) == 0 {
		b.WriteString("  none\n")
	} else {
		for _, ci := range snapshot.CloudInstances {
			update := snapshot.InstanceUpdates[ci.ID]
			b.WriteString("  ")
			b.WriteString(formatCloudSummaryLine(ci, update))
			b.WriteString("\n")
			for _, line := range formatCloudAssignedJobLines(update) {
				b.WriteString("    ")
				b.WriteString(line)
				b.WriteString("\n")
			}
		}
	}

	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("ON-PREM HOSTS (%d active)\n", len(snapshot.OnPremHosts)))
	if len(snapshot.OnPremHosts) == 0 {
		b.WriteString("  none\n")
	} else {
		for _, host := range snapshot.OnPremHosts {
			running, queued := countHostJobStates(host.Jobs)
			if queued > 0 {
				b.WriteString(fmt.Sprintf("  %s  %d running  %d queued\n", host.Name, running, queued))
			} else {
				b.WriteString(fmt.Sprintf("  %s  %d running\n", host.Name, running))
			}
		}
	}

	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("UNPLACED JOBS (%d)\n", len(snapshot.UnplacedJobs)))
	if len(snapshot.UnplacedJobs) == 0 {
		b.WriteString("  none\n")
	} else {
		for _, job := range snapshot.UnplacedJobs {
			b.WriteString(fmt.Sprintf("  #%d  %s  %s  %s\n",
				job.ID,
				job.DirectoryTailDisplay(),
				truncate(job.EffectiveDescription(), 32),
				formatWatchGPUConstraint(job),
			))
		}
	}

	b.WriteString("\n")
	return b.String()
}

func countHostJobStates(jobs []*db.Job) (running, queued int) {
	for _, job := range jobs {
		switch job.EffectiveStatus() {
		case db.StatusQueued:
			queued++
		case db.StatusRunning, db.StatusStarting, db.StatusPaused:
			running++
		}
	}
	return running, queued
}

func formatCloudSummaryLine(ci *db.CloudInstance, update campaign.InstanceUpdate) string {
	job := currentCloudJob(update)
	jobText := "no jobs"
	progressText := ""
	if job != nil {
		jobText = fmt.Sprintf("job#%d %s %s", job.ID, job.DirectoryTailDisplay(), truncate(job.EffectiveDescription(), 28))
		if update.JobProgressID == job.ID && update.JobProgress >= 0 {
			progressText = fmt.Sprintf("  %d%%", update.JobProgress)
		}
	}

	return fmt.Sprintf("%s  %-18s  %-9s  %s%s  %s",
		watchCloudInstanceLabel(ci),
		truncate(ci.DisplayGPUSpec(), 18),
		watchCloudStatus(ci),
		jobText,
		progressText,
		formatWatchCost(ci, update.Instance),
	)
}

func currentCloudJob(update campaign.InstanceUpdate) *db.Job {
	if len(update.Jobs) == 0 {
		return nil
	}

	var queued *db.Job
	for _, job := range update.Jobs {
		displayStatus := campaign.JobDisplayStatus(job, update.JobAttemptOutcomes)
		switch displayStatus {
		case db.StatusRunning:
			return job
		case db.StatusStarting, db.StatusPaused:
			if queued == nil {
				queued = job
			}
		case db.StatusQueued:
			if queued == nil {
				queued = job
			}
		}
	}
	if queued != nil {
		return queued
	}
	return update.Jobs[0]
}

func formatCloudAssignedJobLines(update campaign.InstanceUpdate) []string {
	if len(update.Jobs) == 0 {
		return nil
	}
	lines := make([]string, 0, len(update.Jobs))
	for _, job := range update.Jobs {
		status := campaign.JobDisplayStatus(job, update.JobAttemptOutcomes)
		line := fmt.Sprintf("#%-4d %-16s %-9s %s",
			job.ID,
			truncate(job.DirectoryTailDisplay(), 16),
			status,
			truncate(job.EffectiveDescription(), 36),
		)
		if update.JobProgressID == job.ID && update.JobProgress >= 0 {
			line += fmt.Sprintf("  %d%%", update.JobProgress)
		}
		lines = append(lines, line)
	}
	return lines
}

func watchCloudInstanceLabel(ci *db.CloudInstance) string {
	if providerID := strings.TrimSpace(ci.EffectiveProviderID()); providerID != "" {
		return providerID
	}
	return fmt.Sprintf("#%d", ci.ID)
}

func watchCloudStatus(ci *db.CloudInstance) string {
	if ci.Status == db.CloudInstanceStatusGrace {
		return "grace"
	}
	return ci.Status
}

func formatWatchCost(ci *db.CloudInstance, inst *cloud.Instance) string {
	if ci.ActualSpendCents > 0 {
		return fmt.Sprintf("$%.2f", float64(ci.ActualSpendCents)/100.0)
	}
	if ci.LaunchedAt != nil && inst != nil && inst.CostPerHour > 0 {
		uptime := time.Since(time.Unix(*ci.LaunchedAt, 0))
		return fmt.Sprintf("$%.2f", uptime.Hours()*inst.CostPerHour)
	}
	if ci.CostPerHourCents > 0 && ci.LaunchedAt != nil {
		uptime := time.Since(time.Unix(*ci.LaunchedAt, 0))
		return fmt.Sprintf("$%.2f", uptime.Hours()*float64(ci.CostPerHourCents)/100.0)
	}
	return "--"
}

func formatWatchGPUConstraint(job *db.Job) string {
	switch {
	case job.GPUClass != "" && job.GPUMemGB != nil:
		return fmt.Sprintf("GPU: %s>=%dGB", job.GPUClass, *job.GPUMemGB)
	case job.GPUClass != "":
		return "GPU: " + job.GPUClass
	case job.GPUMemGB != nil:
		return fmt.Sprintf("GPU: >=%dGB", *job.GPUMemGB)
	default:
		return "(no GPU constraint)"
	}
}
