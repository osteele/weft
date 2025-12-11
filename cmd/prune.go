package cmd

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/spf13/cobra"
)

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Remove old jobs from the database",
	Long: `Remove completed and dead jobs from the local database.

By default, removes all completed and dead jobs. Use --older-than to
filter by age.

Examples:
  remote-jobs prune                    # Remove all completed/dead jobs
  remote-jobs prune --older-than 7d    # Only jobs older than 7 days
  remote-jobs prune --older-than 24h   # Only jobs older than 24 hours
  remote-jobs prune --dry-run          # Preview what would be deleted
  remote-jobs prune --dead-only        # Only remove dead jobs`,
	RunE: runPrune,
}

var (
	pruneOlderThan string
	pruneDryRun    bool
	pruneDeadOnly  bool
)

func init() {
	rootCmd.AddCommand(pruneCmd)
	pruneCmd.Flags().StringVar(&pruneOlderThan, "older-than", "", "Only remove jobs older than this duration (e.g., 7d, 24h, 30m)")
	pruneCmd.Flags().BoolVar(&pruneDryRun, "dry-run", false, "Preview without actually deleting")
	pruneCmd.Flags().BoolVar(&pruneDeadOnly, "dead-only", false, "Only remove dead jobs (not completed)")
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

	// Dry run mode - show what would be deleted
	if pruneDryRun {
		jobs, err := db.ListJobsForPrune(database, pruneDeadOnly, olderThan)
		if err != nil {
			return fmt.Errorf("list jobs: %w", err)
		}

		if len(jobs) == 0 {
			fmt.Println("No jobs to prune")
			return nil
		}

		fmt.Printf("Would delete %d job(s):\n\n", len(jobs))
		for _, job := range jobs {
			startTime := time.Unix(job.StartTime, 0)
			fmt.Printf("  ID %d: %s/%s (%s) - %s\n",
				job.ID, job.Host, job.SessionName, job.Status,
				startTime.Format("2006-01-02 15:04"))
		}
		return nil
	}

	// Actually prune
	count, err := db.PruneJobs(database, pruneDeadOnly, olderThan)
	if err != nil {
		return fmt.Errorf("prune jobs: %w", err)
	}

	if count == 0 {
		fmt.Println("No jobs to prune")
	} else {
		what := "completed and dead"
		if pruneDeadOnly {
			what = "dead"
		}
		fmt.Printf("Pruned %d %s job(s)\n", count, what)
	}

	return nil
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
