package cmd

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove old jobs from the database and remote files",
	Long: `Remove completed and dead jobs from the local database and their
log files from remote hosts.

By default, removes all completed and dead jobs. Use --older-than to
filter by age.

Examples:
  remote-jobs prune                    # Remove all completed/dead jobs
  remote-jobs prune --older-than 7d    # Only jobs older than 7 days
  remote-jobs prune --older-than 24h   # Only jobs older than 24 hours
  remote-jobs prune --dry-run          # Preview what would be deleted
  remote-jobs prune --dead-only        # Only remove dead jobs
  remote-jobs prune --keep-files       # Don't delete remote files`,
	RunE: runPrune,
}

var (
	pruneOlderThan string
	pruneDryRun    bool
	pruneDeadOnly  bool
	pruneKeepFiles bool
)

func init() {
	rootCmd.AddCommand(pruneCmd)
	pruneCmd.Flags().StringVar(&pruneOlderThan, "older-than", "", "Only remove jobs older than this duration (e.g., 7d, 24h, 30m)")
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "Preview without actually deleting")
	pruneCmd.Flags().BoolVar(&pruneDeadOnly, "dead-only", false, "Only remove dead jobs (not completed)")
	pruneCmd.Flags().BoolVar(&pruneKeepFiles, "keep-files", false, "Don't delete remote log files")
}

func runPrune(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Parse duration if specified
	var olderThan *time.Time
	if pruneOlderThan != "" {
		duration, err := parseDuration(pruneOlderThan)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w (examples: 7d, 24h, 30m)", pruneOlderThan, err)
		}
		cutoff := time.Now().Add(-duration)
		olderThan = &cutoff
	}

	// Get jobs to be pruned (needed for both dry-run and actual deletion)
	jobs, err := db.ListJobsForPrune(database, pruneDeadOnly, olderThan)
	if err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}

	if len(jobs) == 0 {
		fmt.Println("No jobs to prune")
		return nil
	}

	// Dry run mode - show what would be deleted
	if pruneDryRun {
		fmt.Printf("Would delete %d job(s):\n\n", len(jobs))
		for _, job := range jobs {
			startTime := time.Unix(job.StartTime, 0)
			display := job.Description
			if display == "" {
				display = job.Command
				if len(display) > 30 {
					display = display[:27] + "..."
				}
			}
			fmt.Printf("  ID %d: %s (%s) - %s - %s\n",
				job.ID, job.Host, job.Status,
				startTime.Format("2006-01-02 15:04"), display)
		}
		if !pruneKeepFiles {
			fmt.Println("\n(Would also delete associated log files on remote hosts)")
		}
		return nil
	}

	// Delete remote files first (before removing from DB)
	if !pruneKeepFiles {
		filesDeleted := 0
		for _, job := range jobs {
			deleted := deleteJobFiles(job)
			if deleted {
				filesDeleted++
			}
		}
		if filesDeleted > 0 {
			fmt.Printf("Deleted log files for %d job(s)\n", filesDeleted)
		}
	}

	// Actually prune from database
	count, err := db.PruneJobs(database, pruneDeadOnly, olderThan)
	if err != nil {
		return fmt.Errorf("prune jobs: %w", err)
	}

	what := "completed and dead"
	if pruneDeadOnly {
		what = "dead"
	}
	fmt.Printf("Pruned %d %s job(s) from database\n", count, what)

	return nil
}

// deleteJobFiles deletes all files associated with a job on the remote host
// Returns true if any files were deleted
func deleteJobFiles(job *db.Job) bool {
	// Determine file paths based on whether this is an old or new job
	var logFile, statusFile, metadataFile string
	if job.SessionName != "" {
		// Old job with session name - use legacy paths
		logFile = session.LegacyLogFile(job.SessionName)
		statusFile = session.LegacyStatusFile(job.SessionName)
		metadataFile = session.LegacyMetadataFile(job.SessionName)
	} else {
		// New job - use ID-based paths
		logFile = session.LogFile(job.ID, job.StartTime)
		statusFile = session.StatusFile(job.ID, job.StartTime)
		metadataFile = session.MetadataFile(job.ID, job.StartTime)
	}

	// Build delete command
	deleteCmd := fmt.Sprintf("rm -f '%s' '%s' '%s' 2>/dev/null", logFile, statusFile, metadataFile)

	// Try to delete - silently ignore connection errors
	_, _, err := ssh.Run(job.Host, deleteCmd)
	if err != nil {
		// Check if it's a connection error (silently ignore)
		if ssh.IsConnectionError(err.Error()) {
			return false
		}
		fmt.Fprintf(os.Stderr, "Warning: failed to delete files for job %d on %s: %v\n", job.ID, job.Host, err)
		return false
	}

	return true
}

// parseDuration parses a duration string, supporting "d" suffix for days
func parseDuration(s string) (time.Duration, error) {
	// Handle "d" suffix for days (Go's time.ParseDuration doesn't support days)
	re := regexp.MustCompile(`^(\d+)d$`)
	if matches := re.FindStringSubmatch(s); matches != nil {
		days, _ := strconv.Atoi(matches[1])
		return time.Duration(days) * 24 * time.Hour, nil
	}
	// Fall back to standard Go duration parsing (handles h, m, s)
	return time.ParseDuration(s)
}
