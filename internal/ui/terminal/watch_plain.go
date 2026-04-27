package terminal

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/degraded"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/logging"
	"github.com/osteele/weft/internal/orchestration"
	"github.com/osteele/weft/internal/queueblock"
)

type onPremHostSummary struct {
	Name string
	Jobs []*db.Job
}

type watchSystemSnapshot struct {
	Launches        []*db.Launch
	InstanceUpdates map[int64]campaign.InstanceUpdate
	OnPremHosts     []onPremHostSummary
	UnplacedJobs    []*db.Job
	CloudDegraded   bool
	CloudReason     string
}

func watchAllPlain(database *sql.DB, cfg *config.Config, opts WatchPlainOptions) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	reconciler := campaign.NewReconciler()
	var previousLaunchIDs []int64

	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	var tracker *TransitionTracker
	if opts.EmitsEvents() {
		tracker = NewTransitionTracker()
	}

	for {
		performFastSync(database, false)
		snapshot, err := loadWatchSystemSnapshot(database, cfg, reconciler, true, previousLaunchIDs)
		if err != nil {
			return err
		}
		previousLaunchIDs = launchIDs(snapshot.Launches)
		now := time.Now()

		jobs := flattenSystemSnapshotJobs(snapshot)

		switch opts.SnapshotMode() {
		case SnapshotText:
			fmt.Fprint(stdout, formatWatchPlainSnapshot(snapshot, now))
		case SnapshotJSON:
			if err := opts.EmitSnapshotJSON(jobs, now); err != nil {
				return err
			}
		case SnapshotNone:
		}

		var anyTerminalTransition bool
		if tracker != nil {
			events := tracker.Diff(jobs, now)
			anyTerminalTransition, err = opts.EmitTransitions(events)
			if err != nil {
				return err
			}
		}

		if opts.UntilAnyTerminal && anyTerminalTransition {
			return nil
		}
		if !opts.Follow && !opts.UntilAnyTerminal && snapshot.IsEmpty() {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(TerminalSyncInterval):
		}
	}
}

// flattenSystemSnapshotJobs returns one *db.Job per unique job ID across all
// of a system snapshot's buckets (cloud-instance jobs, on-prem jobs,
// unplaced jobs). Cloud-instance Jobs lists may include attempt history;
// the latest *db.Job for each ID wins.
func flattenSystemSnapshotJobs(snapshot watchSystemSnapshot) []*db.Job {
	by := make(map[int64]*db.Job)
	for _, upd := range snapshot.InstanceUpdates {
		for _, j := range upd.Jobs {
			if j == nil {
				continue
			}
			by[j.ID] = j
		}
	}
	for _, host := range snapshot.OnPremHosts {
		for _, j := range host.Jobs {
			if j == nil {
				continue
			}
			by[j.ID] = j
		}
	}
	for _, j := range snapshot.UnplacedJobs {
		if j == nil {
			continue
		}
		by[j.ID] = j
	}
	out := make([]*db.Job, 0, len(by))
	for _, j := range by {
		out = append(out, j)
	}
	return out
}

func loadWatchSystemSnapshot(database *sql.DB, cfg *config.Config, reconciler *campaign.Reconciler, refresh bool, previousLaunchIDs []int64) (watchSystemSnapshot, error) {
	cloudDegraded := false
	cloudReason := ""
	if refresh {
		warnings := syncCloudStateTwoPhaseForTUI(database)
		if len(warnings) > 0 {
			cloudDegraded = true
			cloudReason = strings.Join(warnings, " | ")
		}
	}

	cloudInstances, err := db.ListRunningLaunches(database)
	if err != nil {
		return watchSystemSnapshot{}, fmt.Errorf("list running cloud instances: %w", err)
	}
	if len(cloudInstances) == 0 && len(previousLaunchIDs) > 0 {
		cloudInstances, err = reloadActiveLaunches(database, previousLaunchIDs)
		if err != nil {
			return watchSystemSnapshot{}, err
		}
		if len(cloudInstances) > 0 {
			cloudDegraded = true
			cloudReason = degraded.CloudLastKnownRentalsReason()
		}
	}

	instanceUpdates := make(map[int64]campaign.InstanceUpdate, len(cloudInstances))
	for _, ci := range cloudInstances {
		jobs, err := db.GetLaunchJobsIncludingAttempts(database, ci.ID)
		if err != nil {
			return watchSystemSnapshot{}, fmt.Errorf("list jobs for cloud instance %s: %w", ids.FormatInstanceID(ci.ID), err)
		}
		outcomes, err := db.GetAttemptOutcomesByLaunch(database, ci.ID)
		if err != nil {
			return watchSystemSnapshot{}, fmt.Errorf("list attempt outcomes for cloud instance %s: %w", ids.FormatInstanceID(ci.ID), err)
		}
		instanceUpdates[ci.ID] = campaign.InstanceUpdate{
			Launch:             ci,
			Jobs:               jobs,
			JobAttemptOutcomes: outcomes,
		}
	}

	onPremJobs, err := db.ListActiveOnPremJobs(database)
	if err != nil {
		return watchSystemSnapshot{}, fmt.Errorf("list active on-prem jobs: %w", err)
	}
	queueblock.Apply(onPremJobs, queueblock.Fetch(onPremJobs, 5*time.Second))

	unplacedJobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return watchSystemSnapshot{}, fmt.Errorf("list unplaced jobs: %w", err)
	}
	orchestration.HydrateRelaunchBlockedReasons(database, unplacedJobs)

	return watchSystemSnapshot{
		Launches:        cloudInstances,
		InstanceUpdates: instanceUpdates,
		OnPremHosts:     groupOnPremHosts(onPremJobs),
		UnplacedJobs:    unplacedJobs,
		CloudDegraded:   cloudDegraded,
		CloudReason:     cloudReason,
	}, nil
}

func launchIDs(launches []*db.Launch) []int64 {
	ids := make([]int64, 0, len(launches))
	for _, launch := range launches {
		if launch == nil {
			continue
		}
		ids = append(ids, launch.ID)
	}
	return ids
}

func reloadActiveLaunches(database *sql.DB, launchIDs []int64) ([]*db.Launch, error) {
	launches := make([]*db.Launch, 0, len(launchIDs))
	for _, launchID := range launchIDs {
		launch, err := db.GetLaunch(database, launchID)
		if err != nil {
			return nil, fmt.Errorf("reload cloud instance %s: %w", ids.FormatInstanceID(launchID), err)
		}
		if launch == nil {
			continue
		}
		if !isActiveCloudLaunchStatus(launch.Status) {
			continue
		}
		launches = append(launches, launch)
	}
	return launches, nil
}

func isActiveCloudLaunchStatus(status string) bool {
	switch status {
	case db.LaunchStatusRunning, db.LaunchStatusLaunching, db.LaunchStatusGrace:
		return true
	default:
		return false
	}
}

func (s watchSystemSnapshot) IsEmpty() bool {
	return len(s.Launches) == 0 && len(s.OnPremHosts) == 0 && len(s.UnplacedJobs) == 0
}

func groupOnPremHosts(jobs []*db.Job) []onPremHostSummary {
	grouped := make(map[string][]*db.Job)
	for _, job := range jobs {
		if job == nil || !job.HasInventoryHost() {
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

	b.WriteString(fmt.Sprintf("RENTAL INSTANCES (%d)\n", len(snapshot.Launches)))
	if snapshot.CloudDegraded && snapshot.CloudReason != "" {
		b.WriteString(fmt.Sprintf("  note: %s\n", snapshot.CloudReason))
	}
	if len(snapshot.Launches) == 0 {
		b.WriteString("  none\n")
	} else {
		views := make([]cloudInstanceView, 0, len(snapshot.Launches))
		for _, ci := range snapshot.Launches {
			update := normalizeWatchInstanceUpdate(snapshot.InstanceUpdates[ci.ID], ci)
			views = append(views, cloudInstanceView{
				Launch:   update.Launch,
				Instance: update.Instance,
			})
		}
		if summary := formatCloudAggregateSummary("  Summary:", summarizeLaunches(views, now)); summary != "" {
			b.WriteString(summary)
			b.WriteString("\n\n")
		}
		for i, ci := range snapshot.Launches {
			if i > 0 {
				b.WriteString("\n")
			}
			update := normalizeWatchInstanceUpdate(snapshot.InstanceUpdates[ci.ID], ci)
			b.WriteString(formatWatchInstanceBlock(update, nil, watchInstanceBlockOptions{
				plain: true,
				now:   now,
			}))
			b.WriteString("\n")
		}
	}

	b.WriteString("\n")
	b.WriteString(fmt.Sprintf("INVENTORY HOSTS (%d active)\n", len(snapshot.OnPremHosts)))
	if len(snapshot.OnPremHosts) == 0 {
		b.WriteString("  none\n")
	} else {
		for _, host := range snapshot.OnPremHosts {
			running, queued, blocked := countHostJobStates(host.Jobs)
			switch {
			case blocked > 0 && queued > 0:
				b.WriteString(fmt.Sprintf("  %s  %d running  %d blocked  %d queued\n", host.Name, running, blocked, queued))
			case blocked > 0:
				b.WriteString(fmt.Sprintf("  %s  %d running  %d blocked\n", host.Name, running, blocked))
			case queued > 0:
				b.WriteString(fmt.Sprintf("  %s  %d running  %d queued\n", host.Name, running, queued))
			default:
				b.WriteString(fmt.Sprintf("  %s  %d running\n", host.Name, running))
			}
			for _, job := range host.Jobs {
				display := queueblock.Display(job, nil)
				if !display.Blocked {
					continue
				}
				b.WriteString(fmt.Sprintf("    #%d  blocked  %s\n", job.ID, display.Reason))
			}
		}
	}

	b.WriteString("\n")
	b.WriteString(formatUnplacedJobsSection(snapshot.UnplacedJobs))

	b.WriteString("\n")
	return b.String()
}

// formatUnplacedJobsSection renders a plain-text "UNPLACED JOBS" block.
func formatUnplacedJobsSection(jobs []*db.Job) string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("UNPLACED JOBS (%d)\n", len(jobs)))
	if len(jobs) == 0 {
		b.WriteString("  none\n")
	} else {
		for _, job := range jobs {
			b.WriteString(fmt.Sprintf("  #%d  %s  %s  %s\n",
				job.ID,
				campaign.JobProjectLabel(job),
				truncate(job.EffectiveDescription(), 32),
				formatWatchGPUConstraint(job),
			))
		}
	}
	return b.String()
}

func countHostJobStates(jobs []*db.Job) (running, queued, blocked int) {
	for _, job := range jobs {
		switch queueblock.Display(job, nil).Status {
		case "blocked":
			blocked++
		case db.StatusQueued:
			queued++
		case db.StatusRunning, db.StatusStarting, db.StatusPaused:
			running++
		}
	}
	return running, queued, blocked
}

// watchJobsPlain watches specific jobs by ID, printing their status periodically
// until all reach a terminal state (or indefinitely with opts.Follow).
func watchJobsPlain(database *sql.DB, jobIDs []int64, opts WatchPlainOptions) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	var tracker *TransitionTracker
	if opts.EmitsEvents() {
		tracker = NewTransitionTracker()
	}

	var lastIssues string

	for {
		warnings := syncWatchedJobHostsQuiet(database, jobIDs)
		now := time.Now()

		watchedJobs := make([]*db.Job, 0, len(jobIDs))
		for _, jobID := range jobIDs {
			job, err := db.GetJobByID(database, jobID)
			if err != nil {
				fmt.Fprintf(stderr, "Job %d: %v\n", jobID, err)
				continue
			}
			if job == nil {
				fmt.Fprintf(stderr, "Job %d not found\n", jobID)
				continue
			}
			watchedJobs = append(watchedJobs, job)
		}
		queueblock.Apply(watchedJobs, queueblock.Fetch(watchedJobs, 5*time.Second))

		allTerminal := true
		for _, job := range watchedJobs {
			if !isTerminalStatus(job.EffectiveStatus()) {
				allTerminal = false
			}
		}

		switch opts.SnapshotMode() {
		case SnapshotText:
			for i, job := range watchedJobs {
				if i > 0 {
					fmt.Fprintln(stdout, "---")
				}
				printJobStatus(job, false)
			}
		case SnapshotJSON:
			if err := opts.EmitSnapshotJSON(watchedJobs, now); err != nil {
				return err
			}
		case SnapshotNone:
		}

		var anyTerminalTransition bool
		if tracker != nil {
			events := tracker.Diff(watchedJobs, now)
			var err error
			anyTerminalTransition, err = opts.EmitTransitions(events)
			if err != nil {
				return err
			}
		}

		if len(warnings) > 0 {
			issueBlock := formatIssuesBlock(warnings)
			if issueBlock != lastIssues {
				fmt.Fprint(stderr, issueBlock)
				lastIssues = issueBlock
			}
		} else {
			lastIssues = ""
		}

		if opts.UntilAnyTerminal && anyTerminalTransition {
			return nil
		}
		if !opts.Follow && !opts.UntilAnyTerminal && allTerminal {
			return nil
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(TerminalSyncInterval):
		}
	}
}

// syncWatchedJobHostsQuiet syncs hosts for the given jobs, suppressing log noise
// and returning actionable warnings as structured strings.
func syncWatchedJobHostsQuiet(database *sql.DB, jobIDs []int64) []string {
	hosts := make(map[string]struct{})
	needsRentalSync := false
	for _, jobID := range jobIDs {
		job, err := db.GetJobByID(database, jobID)
		if err != nil || job == nil || isTerminalStatus(job.EffectiveStatus()) {
			continue
		}
		if job.HasInventoryHost() {
			hosts[job.Host] = struct{}{}
		}
		if job.IsRentalJob() {
			needsRentalSync = true
		}
	}

	var warnings []string

	// Capture warning-level log messages from sync internals.
	ch := logging.NewCapturingHandler(slog.LevelWarn)
	prev := slog.Default()
	slog.SetDefault(slog.New(ch))

	if len(hosts) > 0 {
		hostList := make([]string, 0, len(hosts))
		for h := range hosts {
			hostList = append(hostList, h)
		}
		_, _, _, hostWarnings := performSyncWithTimeoutForHostsDetailed(database, hostList, FastSyncTimeout, false)
		warnings = append(warnings, hostWarnings...)
	}
	if needsRentalSync {
		syncRentalJobsStatus(database)
	}

	slog.SetDefault(prev)
	warnings = append(warnings, ch.Messages()...)
	return warnings
}

// formatIssuesBlock renders warnings as a visually distinct block.
func formatIssuesBlock(warnings []string) string {
	var sb strings.Builder
	sb.WriteString("\nIssues:\n")
	for _, w := range warnings {
		sb.WriteString("  * ")
		sb.WriteString(w)
		sb.WriteString("\n")
	}
	return sb.String()
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
