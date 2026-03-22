package cmd

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/remediation"
	"github.com/osteele/weft/internal/ssh"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list [jobs|campaigns|instances|hosts|queues|artifacts|projects] [flags]",
	Short: "List jobs, campaigns, instances, hosts, queues, artifacts, or projects",
	Long: `List resources. Without a subcommand, lists jobs (same as "weft list jobs").

Subcommands:
  jobs        List and search job history (default)
  campaigns   List campaigns with instance counts
  instances   List cloud instances
  hosts       List known hosts
  queues      List job queues
  artifacts   List job output artifacts
  projects    List projects with summary stats

Examples:
  weft list                    # Recent jobs (last 7 days)
  weft list jobs --running     # Running jobs only
  weft list campaigns          # All campaigns
  weft list instances          # All cloud instances
  weft list hosts              # Known hosts
  weft list projects           # Projects with job counts
  weft list --all              # All jobs including older
  weft list --queued           # Jobs waiting in queue`,
	RunE: runList,
}

var (
	listRunning     bool
	listCompleted   bool
	listQueued      bool
	listDead        bool
	listFailed      bool
	listStatus      string
	listHost        string
	listAllHosts    bool
	listSearch      string
	listLimit       int
	listShow        int64
	listCleanup     int
	listSync        bool
	listNoSync      bool
	listAll         bool
	listTags        []string
	listExcludeTags []string
	listProject     string
	listProcessed   bool
	listUnprocessed bool
	listRental      bool
	listInventory   bool
	listCloud       bool
	listTUI         bool
	listPlain       bool
)

const defaultHostSyncWindow = 48 * time.Hour

// addListQueryFlags registers list query/filter flags shared by job list and
// project jobs.
func addListQueryFlags(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&listRunning, "running", false, "Show only running jobs")
	cmd.Flags().BoolVar(&listCompleted, "completed", false, "Show only completed jobs")
	cmd.Flags().BoolVar(&listQueued, "queued", false, "Show only queued jobs (waiting in queue)")
	cmd.Flags().BoolVar(&listDead, "dead", false, "Show only dead jobs")
	cmd.Flags().BoolVar(&listFailed, "failed", false, "Show only failed jobs (failed, dead, or completed with non-zero exit code)")
	cmd.Flags().StringVarP(&listStatus, "status", "s", "", "Filter by status (running, paused, starting, completed, queued, dead, failed, processed, unprocessed)")
	cmd.Flags().StringVar(&listHost, "host", "", "Filter by host")
	cmd.Flags().BoolVar(&listAllHosts, "all-hosts", false, "Include jobs from hosts not synced recently")
	cmd.Flags().StringVar(&listSearch, "search", "", "Search by description or command")
	cmd.Flags().StringVar(&listSearch, "filter", "", "Search by description or command (alias for --search)")
	cmd.Flags().StringSliceVar(&listTags, "tag", nil, "Filter by tag (can be repeated)")
	cmd.Flags().StringSliceVar(&listExcludeTags, "exclude-tag", nil, "Exclude jobs with tag (can be repeated)")
	cmd.Flags().StringVar(&listProject, "project", "", "Filter by project name")
	cmd.Flags().BoolVar(&listProcessed, "processed", false, "Show only jobs with the processed tag")
	cmd.Flags().BoolVar(&listUnprocessed, "unprocessed", false, "Show only jobs without the processed tag")
	cmd.Flags().BoolVar(&listRental, "rental", false, "Show jobs tagged for rental placement or assigned to rental instances")
	cmd.Flags().BoolVar(&listInventory, "inventory", false, "Show inventory-only jobs and jobs assigned to inventory hosts")
	cmd.Flags().BoolVar(&listCloud, "cloud", false, "Alias for --rental")
	cmd.Flags().MarkHidden("cloud")
	cmd.Flags().IntVar(&listLimit, "limit", 50, "Limit results")
	cmd.Flags().BoolVar(&listSync, "sync", false, "Perform full sync (default is fast sync with timeout)")
	cmd.Flags().BoolVar(&listNoSync, "no-sync", false, "Skip syncing job statuses before listing")
	cmd.Flags().BoolVarP(&listAll, "all", "a", false, "Include jobs older than 7 days")
}

// addListFlags registers all list-related flags on a command.
// Used by both listCmd and jobListCmd to share the same flag definitions.
func addListFlags(cmd *cobra.Command) {
	addListQueryFlags(cmd)
	cmd.Flags().Int64Var(&listShow, "show", 0, "Show detailed info for a specific job ID")
	cmd.Flags().IntVar(&listCleanup, "cleanup", 0, "Delete jobs older than N days")
	cmd.Flags().BoolVar(&listTUI, "tui", false, "Force interactive TUI mode")
	cmd.Flags().BoolVar(&listPlain, "plain", false, "Force plain text output")
	cmd.MarkFlagsMutuallyExclusive("tui", "plain")
}

func init() {
	rootCmd.AddCommand(listCmd)
	addListFlags(listCmd)
}

func runList(cmd *cobra.Command, args []string) error {
	useTUI := false
	if listCleanup == 0 && listShow == 0 {
		var err error
		useTUI, err = resolveCampaignTUI(listTUI, listPlain)
		if err != nil {
			return err
		}
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// TUI shows current DB state immediately and syncs in the background.
	if !useTUI {
		for _, warning := range syncListData(database) {
			fmt.Fprintln(os.Stderr, warning)
		}
	}

	// Handle cleanup mode
	if listCleanup > 0 {
		deleted, err := db.CleanupOld(database, listCleanup)
		if err != nil {
			return fmt.Errorf("cleanup: %w", err)
		}
		fmt.Printf("Deleted %d jobs older than %d days\n", deleted, listCleanup)
		return nil
	}

	// Handle show single job
	if listShow > 0 {
		return showJob(database, listShow)
	}

	jobs, err := collectJobsForList(database, args)
	if err != nil {
		return err
	}

	if useTUI {
		return runListTUI(database, args, jobs, buildListTitle(args), !listNoSync)
	}
	return printJobs(database, jobs)
}

func syncListData(database *sql.DB) []string {
	if listNoSync {
		return nil
	}

	var warnings []string
	if listSync {
		if listHost != "" {
			if err := performListSyncForHost(database, listHost); err != nil {
				warnings = append(warnings, fmt.Sprintf("Warning: sync failed: %v", err))
			}
		} else {
			if err := performListSync(database); err != nil {
				warnings = append(warnings, fmt.Sprintf("Warning: sync failed: %v", err))
			}
		}
		return warnings
	}

	var completed bool
	var unreachable []string
	var syncWarnings []string
	if listHost != "" {
		completed, unreachable, syncWarnings = performSyncWithTimeoutForHostsDetailed(database, []string{listHost}, FastSyncTimeout, false)
	} else {
		completed, unreachable, syncWarnings = performSyncWithTimeoutForHostsDetailed(database, nil, FastSyncTimeout, false)
	}
	warnings = append(warnings, syncWarnings...)
	if !completed {
		if note := buildStaleDataNote(database, unreachable); note != "" {
			warnings = append(warnings, note)
		}
	}
	return warnings
}

func collectJobsForList(database *sql.DB, args []string) ([]*db.Job, error) {
	statusFilter, processedFilter, failedOnly, err := listFilters()
	if err != nil {
		return nil, err
	}
	wantRental, wantInventory, err := listPlacementFlags()
	if err != nil {
		return nil, err
	}

	// Handle explicit job IDs: weft list 12::14, weft list 12...13, weft list 12,13,14
	if len(args) > 0 {
		jobIDs, err := ParseJobIDs(args)
		if err != nil {
			return nil, err
		}
		jobs, missingIDs, err := listJobsByID(database, jobIDs)
		if err != nil {
			return nil, err
		}
		if len(missingIDs) > 0 {
			fmt.Fprintf(os.Stderr, "Warning: job(s) not found: %s\n", formatJobIDList(missingIDs))
		}
		jobs = filterJobsForListArgs(jobs, statusFilter, processedFilter, failedOnly, wantRental, wantInventory)
		if listLimit > 0 && len(jobs) > listLimit {
			jobs = jobs[:listLimit]
		}
		return jobs, nil
	}

	hostFilterHosts := []string{}
	if listHost != "" {
		hostFilterHosts = []string{listHost}
	} else if listQueued {
		// --queued is an explicit request for all pending jobs; unplaced jobs have no
		// host, so filtering by recently-synced hosts would silently hide them.
	} else if !listAllHosts {
		recentHosts, err := db.ListHostsSyncedSince(database, time.Now().Add(-defaultHostSyncWindow))
		if err != nil {
			return nil, fmt.Errorf("list recent hosts: %w", err)
		}
		if len(recentHosts) > 0 {
			hostFilterHosts = recentHosts
		}
	}

	// Handle search
	if listSearch != "" {
		searchLimit := listLimit
		if len(listTags) > 0 || processedFilter != "" || failedOnly || len(listExcludeTags) > 0 || len(hostFilterHosts) > 0 || listProject != "" || wantRental || wantInventory {
			searchLimit = 0
		}
		jobs, err := db.SearchJobs(database, listSearch, searchLimit)
		if err != nil {
			return nil, fmt.Errorf("search: %w", err)
		}
		jobs = jobsWithEffectiveStatus(jobs, statusFilter)
		jobs = filterJobsByFailureState(jobs, failedOnly)
		jobs = db.FilterJobsByTags(jobs, listTags, processedFilter)
		jobs = db.FilterJobsByHosts(jobs, hostFilterHosts)
		jobs = db.FilterJobsByExcludedTags(jobs, listExcludeTags)
		jobs = db.FilterJobsByProject(jobs, listProject)
		jobs = filterJobsByPlacementScope(jobs, wantRental, wantInventory)
		if listLimit > 0 && len(jobs) > listLimit {
			jobs = jobs[:listLimit]
		}
		return jobs, nil
	}

	// Default to 7 days unless --all is specified
	maxAgeDays := 7
	if listAll {
		maxAgeDays = 0
	}

	queryLimit := listLimit
	if len(listTags) > 0 || processedFilter != "" || failedOnly || len(listExcludeTags) > 0 || listProject != "" || wantRental || wantInventory {
		queryLimit = 0
	}
	jobs, err := db.ListJobsWithMaxAgeForHosts(database, statusFilter, hostFilterHosts, queryLimit, maxAgeDays, listTags, processedFilter)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	jobs = jobsWithEffectiveStatus(jobs, statusFilter)
	jobs = filterJobsByFailureState(jobs, failedOnly)
	jobs = db.FilterJobsByExcludedTags(jobs, listExcludeTags)
	jobs = db.FilterJobsByProject(jobs, listProject)
	jobs = filterJobsByPlacementScope(jobs, wantRental, wantInventory)
	if listLimit > 0 && len(jobs) > listLimit {
		jobs = jobs[:listLimit]
	}

	return jobs, nil
}

func listFilters() (statusFilter, processedFilter string, failedOnly bool, err error) {
	if listStatus == "processed" || listStatus == "unprocessed" {
		processedFilter = listStatus
	}
	if listProcessed {
		if processedFilter == "unprocessed" {
			return "", "", false, usageErrorf("--processed and --unprocessed are mutually exclusive")
		}
		processedFilter = "processed"
	}
	if listUnprocessed {
		if processedFilter == "processed" {
			return "", "", false, usageErrorf("--processed and --unprocessed are mutually exclusive")
		}
		processedFilter = "unprocessed"
	}
	if listStatus != "" && processedFilter == "" {
		statusFilter = listStatus
	}
	if listRunning {
		statusFilter = db.StatusRunning
	} else if listCompleted {
		statusFilter = db.StatusCompleted
	} else if listQueued {
		statusFilter = db.StatusQueued
	} else if listDead {
		statusFilter = db.StatusDead
	}
	return statusFilter, processedFilter, listFailed, nil
}

func listJobsByID(database *sql.DB, ids []int64) ([]*db.Job, []int64, error) {
	jobs := make([]*db.Job, 0, len(ids))
	missing := make([]int64, 0)
	for _, id := range ids {
		job, err := db.GetJobByID(database, id)
		if err != nil {
			return nil, nil, fmt.Errorf("get job %d: %w", id, err)
		}
		if job == nil || job.Tombstoned {
			missing = append(missing, id)
			continue
		}
		jobs = append(jobs, job)
	}
	return jobs, missing, nil
}

func filterJobsForListArgs(jobs []*db.Job, statusFilter, processedFilter string, failedOnly, wantRental, wantInventory bool) []*db.Job {
	jobs = jobsWithEffectiveStatus(jobs, statusFilter)
	jobs = filterJobsByFailureState(jobs, failedOnly)

	if listSearch != "" {
		filter := strings.ToLower(listSearch)
		filtered := make([]*db.Job, 0, len(jobs))
		for _, job := range jobs {
			if strings.Contains(strings.ToLower(job.EffectiveDescription()), filter) ||
				strings.Contains(strings.ToLower(job.EffectiveCommand()), filter) {
				filtered = append(filtered, job)
			}
		}
		jobs = filtered
	}

	jobs = db.FilterJobsByTags(jobs, listTags, processedFilter)
	jobs = db.FilterJobsByExcludedTags(jobs, listExcludeTags)
	jobs = db.FilterJobsByProject(jobs, listProject)
	jobs = filterJobsByPlacementScope(jobs, wantRental, wantInventory)
	if listHost != "" {
		jobs = db.FilterJobsByHosts(jobs, []string{listHost})
	}
	return jobs
}

func listPlacementFlags() (wantRental, wantInventory bool, err error) {
	wantRental = listRental || listCloud
	wantInventory = listInventory
	if wantRental && wantInventory {
		return false, false, usageErrorf("--rental and --inventory are mutually exclusive")
	}
	return wantRental, wantInventory, nil
}

func filterJobsByPlacementScope(jobs []*db.Job, wantRental, wantInventory bool) []*db.Job {
	if !wantRental && !wantInventory {
		return jobs
	}
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		switch {
		case wantRental && job.UsesRentalPlacement():
			filtered = append(filtered, job)
		case wantInventory && job.UsesInventoryPlacement():
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func filterJobsByFailureState(jobs []*db.Job, failedOnly bool) []*db.Job {
	if !failedOnly {
		return jobs
	}
	filtered := make([]*db.Job, 0, len(jobs))
	for _, job := range jobs {
		if isFailedJob(job) {
			filtered = append(filtered, job)
		}
	}
	return filtered
}

func isFailedJob(job *db.Job) bool {
	if job == nil {
		return false
	}
	switch job.EffectiveStatus() {
	case db.StatusFailed, db.StatusDead:
		return true
	case db.StatusCompleted:
		return job.ExitCode != nil && *job.ExitCode != 0
	default:
		return false
	}
}

func showJob(database *sql.DB, id int64) error {
	job, err := db.GetJobByID(database, id)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %d not found", id)
	}

	fmt.Printf("Job ID:       %d\n", job.ID)
	fmt.Printf("Target:       %s\n", job.TargetDisplay())
	fmt.Printf("Working Dir:  %s\n", job.EffectiveWorkingDir())
	if job.Project != "" {
		fmt.Printf("Project:      %s\n", job.Project)
	}
	fmt.Printf("Command:      %s\n", job.EffectiveCommand())
	if job.Description != "" {
		fmt.Printf("Description:  %s\n", job.Description)
	} else if job.GeneratedDescription != "" {
		fmt.Printf("Description:  %s (AI-generated)\n", job.GeneratedDescription)
	}
	if tags := job.DisplayTags(); len(tags) > 0 {
		fmt.Printf("Tags:         %s\n", strings.Join(tags, ", "))
	}
	fmt.Printf("Status:       %s\n", job.EffectiveStatus())
	fmt.Printf("Start Time:   %s\n", time.Unix(job.StartTime, 0).Format("2006-01-02 15:04:05"))
	if job.EndTime != nil {
		fmt.Printf("End Time:     %s\n", time.Unix(*job.EndTime, 0).Format("2006-01-02 15:04:05"))
		duration := *job.EndTime - job.StartTime
		fmt.Printf("Duration:     %s\n", db.FormatDuration(duration))
	}
	if job.ExitCode != nil {
		fmt.Printf("Exit Code:    %d\n", *job.ExitCode)
	}
	if job.ErrorDiagnosis != "" {
		d, err := remediation.UnmarshalDiagnosis(job.ErrorDiagnosis)
		if err == nil && d != nil {
			fmt.Printf("Diagnosis:    %s (%s/%s)\n", d.Message, d.Category, d.Pattern)
			if job.RetryCount > 0 {
				fmt.Printf("Retried:      %d time(s)\n", job.RetryCount)
			}
		}
	}

	return nil
}

func printJobs(database *sql.DB, jobs []*db.Job) error {
	applyAttemptOutcomeOverrides(database, jobs)
	return writeListPlainOutput(renderJobListPlain(jobs, listOutputWidth()))
}

// applyAttemptOutcomeOverrides overrides the display status for queued jobs
// whose latest cloud attempt outcome is "failed". These are jobs that ran
// and failed on a cloud instance, then were reset to queued — they should
// display as "failed" rather than "queued".
func applyAttemptOutcomeOverrides(database *sql.DB, jobs []*db.Job) {
	if database == nil {
		return
	}
	for _, job := range jobs {
		if job == nil || job.Status != db.StatusQueued {
			continue
		}
		outcome := db.GetLatestAttemptOutcome(database, job.ID)
		if outcome == db.AttemptOutcomeFailed {
			job.Status = db.StatusFailed
		}
	}
}

func buildListTitle(args []string) string {
	parts := []string{"Jobs"}
	if len(args) > 0 {
		parts = append(parts, "selection")
	}
	if listHost != "" {
		parts = append(parts, "host="+listHost)
	}
	status, processed, failedOnly, err := listFilters()
	if err != nil {
		return "Jobs"
	}
	if status != "" {
		parts = append(parts, "status="+status)
	}
	if processed != "" {
		parts = append(parts, processed)
	}
	wantRental, wantInventory, err := listPlacementFlags()
	if err == nil {
		if wantRental {
			parts = append(parts, db.TagRental)
		}
		if wantInventory {
			parts = append(parts, db.TagInventory)
		}
	}
	if failedOnly {
		parts = append(parts, "failed")
	}
	if status == "" && processed == "" && !failedOnly {
		if listQueued {
			parts = append(parts, "queued")
		}
		if listRunning {
			parts = append(parts, "running")
		}
		if listCompleted {
			parts = append(parts, "completed")
		}
		if listDead {
			parts = append(parts, "dead")
		}
	}
	if listSearch != "" {
		parts = append(parts, "search="+listSearch)
	}
	return strings.Join(parts, " • ")
}

// performListSync runs sync for list --sync flag
func performListSync(database *sql.DB) error {
	hosts, err := db.ListUniqueRunningHosts(database)
	if err != nil {
		return err
	}

	if len(hosts) == 0 {
		return nil
	}

	var updated int
	for _, host := range hosts {
		result, err := syncHost(database, host)
		if err != nil {
			// Silently skip connection errors, warn on others
			if !ssh.IsConnectionError(err.Error()) {
				fmt.Fprintf(os.Stderr, "Warning: error syncing %s: %v\n", host, err)
			}
			continue
		}
		updated += result.Updated
	}

	if updated > 0 {
		fmt.Printf("(synced %d job status(es))\n", updated)
	}

	return nil
}

// performListSyncForHost runs sync for a single host (used with --host flag)
func performListSyncForHost(database *sql.DB, host string) error {
	result, err := syncHost(database, host)
	if err != nil {
		if ssh.IsConnectionError(err.Error()) {
			return nil // Silently skip connection errors
		}
		return err
	}

	if result.Updated > 0 {
		fmt.Printf("(synced %d job status(es))\n", result.Updated)
	}

	return nil
}

// startQueueRunnersForQueuedHosts starts queue runners on hosts that have queue-runner jobs.
func startQueueRunnersForQueuedHosts(database *sql.DB) {
	hosts, err := db.ListHostsWithQueueRunnerJobs(database)
	if err != nil {
		return // Silently ignore errors
	}

	startQueueRunnersForHosts(database, hosts)
}

func startQueueRunnersForHosts(database *sql.DB, hosts []string) {
	hosts = uniqueHosts(hosts)
	if len(hosts) == 0 {
		return
	}
	for _, host := range hosts {
		count, err := db.CountQueueRunnerActiveByHost(database, host)
		if err != nil || count == 0 {
			continue
		}
		_, err = ensureQueueRunnerStarted(host, defaultQueueName)
		if err != nil {
			log.Printf("warning: could not start queue runner on %s: %v", host, err)
			continue
		}
	}
}
