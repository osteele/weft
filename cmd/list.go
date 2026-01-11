package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/queuerunner"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List and search job history",
	Long: `Query and search job history from the local database.

This shows historical jobs, not queue contents. Use --queued to see only jobs
waiting in a queue.

By default, only shows jobs from the last 7 days and hosts synced in the last 2 days.
Use --all/-a to include older jobs and --all-hosts to include older hosts.

Examples:
  remote-jobs list                    # Recent jobs (last 7 days)
  remote-jobs list --all              # All jobs including older
  remote-jobs list --all-hosts        # Include jobs from older hosts
  remote-jobs list --running          # Running jobs only
  remote-jobs list --queued           # Jobs waiting in queue
  remote-jobs list --running --sync   # Running jobs (sync first)
  remote-jobs list --host cool30      # Jobs on cool30
  remote-jobs list --tag exp-012      # Jobs with tag exp-012
  remote-jobs list --exclude-tag exp-012  # Jobs without tag exp-012
  remote-jobs list --status unprocessed --tag exp-012
  remote-jobs list --search training  # Search jobs
  remote-jobs list --show 42          # Job details`,
	RunE: runList,
}

var (
	listRunning     bool
	listCompleted   bool
	listQueued      bool
	listDead        bool
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
)

const defaultHostSyncWindow = 48 * time.Hour

func init() {
	rootCmd.AddCommand(listCmd)

	listCmd.Flags().BoolVar(&listRunning, "running", false, "Show only running jobs")
	listCmd.Flags().BoolVar(&listCompleted, "completed", false, "Show only completed jobs")
	listCmd.Flags().BoolVar(&listQueued, "queued", false, "Show only queued jobs (waiting in queue)")
	listCmd.Flags().BoolVar(&listDead, "dead", false, "Show only dead jobs")
	listCmd.Flags().StringVarP(&listStatus, "status", "s", "", "Filter by status (running, completed, queued, dead, processed, unprocessed)")
	listCmd.Flags().StringVar(&listHost, "host", "", "Filter by host")
	listCmd.Flags().BoolVar(&listAllHosts, "all-hosts", false, "Include jobs from hosts not synced recently")
	listCmd.Flags().StringVar(&listSearch, "search", "", "Search by description or command")
	listCmd.Flags().StringSliceVar(&listTags, "tag", nil, "Filter by tag (can be repeated)")
	listCmd.Flags().StringSliceVar(&listExcludeTags, "exclude-tag", nil, "Exclude jobs with tag (can be repeated)")
	listCmd.Flags().IntVar(&listLimit, "limit", 50, "Limit results")
	listCmd.Flags().Int64Var(&listShow, "show", 0, "Show detailed info for a specific job ID")
	listCmd.Flags().IntVar(&listCleanup, "cleanup", 0, "Delete jobs older than N days")
	listCmd.Flags().BoolVar(&listSync, "sync", false, "Perform full sync (default is fast sync with timeout)")
	listCmd.Flags().BoolVar(&listNoSync, "no-sync", false, "Skip syncing job statuses before listing")
	listCmd.Flags().BoolVarP(&listAll, "all", "a", false, "Include jobs older than 7 days")
}

func runList(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Sync logic: fast sync by default, full sync with --sync, skip with --no-sync
	if !listNoSync {
		if listSync {
			// Full sync requested
			if err := performListSync(database); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: sync failed: %v\n", err)
			}
		} else {
			// Fast sync by default - only sync filtered host if specified
			var completed bool
			var unreachable []string
			if listHost != "" {
				completed, unreachable = performFastSyncForHosts(database, []string{listHost}, false)
			} else {
				completed, unreachable = performFastSync(database, false)
			}
			if !completed {
				if note := buildStaleDataNote(database, unreachable); note != "" {
					fmt.Fprintln(os.Stderr, note)
				}
			}
		}

		// Start queue runners on hosts with queued jobs
		startQueueRunnersForQueuedHosts(database)
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

	processedFilter := ""
	if listStatus == "processed" || listStatus == "unprocessed" {
		processedFilter = listStatus
	}

	hostFilterHosts := []string{}
	if listHost != "" {
		hostFilterHosts = []string{listHost}
	} else if !listAllHosts {
		recentHosts, err := db.ListHostsSyncedSince(database, time.Now().Add(-defaultHostSyncWindow))
		if err != nil {
			return fmt.Errorf("list recent hosts: %w", err)
		}
		if len(recentHosts) > 0 {
			hostFilterHosts = recentHosts
		}
	}

	// Handle search
	if listSearch != "" {
		searchLimit := listLimit
		if len(listTags) > 0 || processedFilter != "" || len(listExcludeTags) > 0 || len(hostFilterHosts) > 0 {
			searchLimit = 0
		}
		jobs, err := db.SearchJobs(database, listSearch, searchLimit)
		if err != nil {
			return fmt.Errorf("search: %w", err)
		}
		jobs = db.FilterJobsByTags(jobs, listTags, processedFilter)
		jobs = db.FilterJobsByHosts(jobs, hostFilterHosts)
		jobs = db.FilterJobsByExcludedTags(jobs, listExcludeTags)
		if listLimit > 0 && len(jobs) > listLimit {
			jobs = jobs[:listLimit]
		}
		return printJobs(jobs)
	}

	// Determine status filter
	var status string
	if listStatus != "" {
		if processedFilter == "" {
			status = listStatus
		}
	} else if listRunning {
		status = db.StatusRunning
	} else if listCompleted {
		status = db.StatusCompleted
	} else if listQueued {
		status = db.StatusQueued
	} else if listDead {
		status = db.StatusDead
	}

	// Default to 7 days unless --all is specified
	maxAgeDays := 7
	if listAll {
		maxAgeDays = 0
	}

	queryLimit := listLimit
	if len(listTags) > 0 || processedFilter != "" || len(listExcludeTags) > 0 {
		queryLimit = 0
	}
	jobs, err := db.ListJobsWithMaxAgeForHosts(database, status, hostFilterHosts, queryLimit, maxAgeDays, listTags, processedFilter)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	jobs = db.FilterJobsByExcludedTags(jobs, listExcludeTags)
	if listLimit > 0 && len(jobs) > listLimit {
		jobs = jobs[:listLimit]
	}

	return printJobs(jobs)
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
	fmt.Printf("Host:         %s\n", job.Host)
	fmt.Printf("Working Dir:  %s\n", job.EffectiveWorkingDir())
	fmt.Printf("Command:      %s\n", job.EffectiveCommand())
	if job.Description != "" {
		fmt.Printf("Description:  %s\n", job.Description)
	} else if job.GeneratedDescription != "" {
		fmt.Printf("Description:  %s (AI-generated)\n", job.GeneratedDescription)
	}
	if len(job.Tags) > 0 {
		fmt.Printf("Tags:         %s\n", strings.Join(job.Tags, ", "))
	}
	fmt.Printf("Status:       %s\n", job.Status)
	fmt.Printf("Start Time:   %s\n", time.Unix(job.StartTime, 0).Format("2006-01-02 15:04:05"))
	if job.EndTime != nil {
		fmt.Printf("End Time:     %s\n", time.Unix(*job.EndTime, 0).Format("2006-01-02 15:04:05"))
		duration := *job.EndTime - job.StartTime
		fmt.Printf("Duration:     %s\n", db.FormatDuration(duration))
	}
	if job.ExitCode != nil {
		fmt.Printf("Exit Code:    %d\n", *job.ExitCode)
	}

	return nil
}

func printJobs(jobs []*db.Job) error {
	if len(jobs) == 0 {
		fmt.Println("No jobs found")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tHOST\tSTATUS\tSTARTED\tDESCRIPTION")

	for _, job := range jobs {
		started := "—"
		if job.StartTime > 0 {
			started = time.Unix(job.StartTime, 0).Format("01/02 15:04")
		}

		status := job.Status
		if job.Status == db.StatusCompleted && job.ExitCode != nil {
			if *job.ExitCode == 0 {
				status = "completed ✓"
			} else {
				status = fmt.Sprintf("failed (%d)", *job.ExitCode)
			}
		}

		// Show description (user or generated), otherwise truncated command
		display := job.EffectiveDescription()
		if len(display) > 50 {
			display = display[:49] + "…"
		}

		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n",
			job.ID, job.Host, status, started, display)
	}

	return w.Flush()
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
		hostUpdated, err := syncHost(database, host)
		if err != nil {
			// Silently skip connection errors, warn on others
			if !ssh.IsConnectionError(err.Error()) {
				fmt.Fprintf(os.Stderr, "Warning: error syncing %s: %v\n", host, err)
			}
			continue
		}
		updated += hostUpdated
	}

	if updated > 0 {
		fmt.Printf("(synced %d job status(es))\n", updated)
	}

	return nil
}

// startQueueRunnersForQueuedHosts starts queue runners on hosts that have queued jobs
func startQueueRunnersForQueuedHosts(database *sql.DB) {
	hosts, err := db.ListHostsWithQueuedJobs(database)
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
		count, err := db.CountQueuedByHost(database, host)
		if err != nil || count == 0 {
			if count == 0 {
				stopQueueRunnerIfIdle(host)
			}
			continue
		}
		started, err := ensureQueueRunnerStarted(host, defaultQueueName)
		if err != nil {
			// Silently ignore - host might be unreachable
			continue
		}
		if started {
			fmt.Printf("(started queue runner on %s)\n", host)
		}
	}
}

func stopQueueRunnerIfIdle(host string) {
	runner := queuerunner.NewRunner(host, defaultQueueName)
	running, err := runner.IsRunning()
	if err != nil || !running {
		return
	}
	_ = runner.SendStopSignal()
}
