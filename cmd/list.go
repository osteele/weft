package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List and search job history",
	Long: `Query and search job history from the local database.

Examples:
  remote-jobs list                    # Recent jobs
  remote-jobs list --running          # Running jobs only
  remote-jobs list --running --sync   # Running jobs (sync first)
  remote-jobs list --pending          # Pending jobs
  remote-jobs list --host cool30      # Jobs on cool30
  remote-jobs list --search training  # Search jobs
  remote-jobs list --show 42          # Job details`,
	RunE: runList,
}

var (
	listRunning   bool
	listCompleted bool
	listDead      bool
	listPending   bool
	listHost      string
	listSearch    string
	listLimit     int
	listShow      int64
	listCleanup   int
	listSync      bool
)

func init() {
	rootCmd.AddCommand(listCmd)

	listCmd.Flags().BoolVar(&listRunning, "running", false, "Show only running jobs")
	listCmd.Flags().BoolVar(&listCompleted, "completed", false, "Show only completed jobs")
	listCmd.Flags().BoolVar(&listDead, "dead", false, "Show only dead jobs")
	listCmd.Flags().BoolVar(&listPending, "pending", false, "Show only pending jobs")
	listCmd.Flags().StringVar(&listHost, "host", "", "Filter by host")
	listCmd.Flags().StringVar(&listSearch, "search", "", "Search by description or command")
	listCmd.Flags().IntVar(&listLimit, "limit", 50, "Limit results")
	listCmd.Flags().Int64Var(&listShow, "show", 0, "Show detailed info for a specific job ID")
	listCmd.Flags().IntVar(&listCleanup, "cleanup", 0, "Delete jobs older than N days")
	listCmd.Flags().BoolVar(&listSync, "sync", false, "Sync job statuses from remote hosts before listing")
}

func runList(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Optional sync before listing
	if listSync {
		if err := performListSync(database); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: sync failed: %v\n", err)
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

	// Handle search
	if listSearch != "" {
		jobs, err := db.SearchJobs(database, listSearch, listLimit)
		if err != nil {
			return fmt.Errorf("search: %w", err)
		}
		return printJobs(jobs)
	}

	// Determine status filter
	var status string
	if listRunning {
		status = db.StatusRunning
	} else if listCompleted {
		status = db.StatusCompleted
	} else if listDead {
		status = db.StatusDead
	} else if listPending {
		status = db.StatusPending
	}

	jobs, err := db.ListJobs(database, status, listHost, listLimit)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
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
	fmt.Fprintln(w, "ID\tHOST\tSTATUS\tSTARTED\tCOMMAND / DESCRIPTION")

	for _, job := range jobs {
		started := time.Unix(job.StartTime, 0).Format("01/02 15:04")

		status := job.Status
		if job.Status == db.StatusCompleted && job.ExitCode != nil {
			if *job.ExitCode == 0 {
				status = "completed ✓"
			} else {
				status = fmt.Sprintf("failed (%d)", *job.ExitCode)
			}
		}

		// Show description if available, otherwise truncated command
		display := job.Description
		if display == "" {
			display = job.EffectiveCommand()
		}
		if len(display) > 40 {
			display = display[:37] + "..."
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
