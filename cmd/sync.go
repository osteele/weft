package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync job statuses from all remote hosts",
	Long: `Sync job statuses by checking all hosts with running jobs.

Automatically finds hosts with running jobs and updates their status
in the local database. Connection failures are silently ignored.

Examples:
  remote-jobs sync              # Sync all hosts
  remote-jobs sync --verbose    # Show progress`,
	RunE: runSync,
}

var syncVerbose bool

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().BoolVarP(&syncVerbose, "verbose", "v", false, "Show detailed progress")
}

func runSync(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get all unique hosts with running jobs
	hosts, err := db.ListUniqueRunningHosts(database)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}

	if len(hosts) == 0 {
		fmt.Println("No running jobs to sync")
		return nil
	}

	var totalUpdated, hostsReached, hostsUnreachable int

	for _, host := range hosts {
		if syncVerbose {
			fmt.Printf("Checking %s...\n", host)
		}

		updated, err := syncHost(database, host)
		if err != nil {
			// Check if it's a connection error
			if ssh.IsConnectionError(err.Error()) {
				hostsUnreachable++
				if syncVerbose {
					fmt.Printf("  %s: unreachable\n", host)
				}
				continue
			}
			// Non-connection error - log warning but continue
			fmt.Fprintf(os.Stderr, "Warning: error syncing %s: %v\n", host, err)
			continue
		}

		hostsReached++
		totalUpdated += updated
		if syncVerbose && updated > 0 {
			fmt.Printf("  %s: %d job(s) updated\n", host, updated)
		}
	}

	// Print summary
	if hostsUnreachable > 0 {
		fmt.Printf("Synced %d job(s) on %d host(s) (%d host(s) unreachable)\n",
			totalUpdated, hostsReached, hostsUnreachable)
	} else {
		fmt.Printf("Synced %d job(s) on %d host(s)\n", totalUpdated, hostsReached)
	}

	return nil
}

// syncHost syncs all running jobs for a host and returns the count of updated jobs
func syncHost(database *sql.DB, host string) (int, error) {
	jobs, err := db.ListRunning(database, host)
	if err != nil {
		return 0, err
	}

	var updated int
	for _, job := range jobs {
		changed, err := syncJob(database, job)
		if err != nil {
			return updated, err
		}
		if changed {
			updated++
		}
	}

	return updated, nil
}

// syncJob checks and updates a single job's status, returning true if status changed
func syncJob(database *sql.DB, job *db.Job) (bool, error) {
	// Check if tmux session still exists (no retry for sync)
	exists, err := ssh.TmuxSessionExistsQuick(job.Host, job.SessionName)
	if err != nil {
		return false, err
	}

	if exists {
		// Job still running, no change
		return false, nil
	}

	// Session doesn't exist - check for status file (no retry for sync)
	statusFile := session.StatusFile(job.SessionName)
	content, err := ssh.ReadRemoteFileQuick(job.Host, statusFile)
	if err != nil {
		return false, err
	}

	if content != "" {
		// Job completed
		exitCode, _ := strconv.Atoi(content)
		endTime := time.Now().Unix()
		if err := db.RecordCompletion(database, job.Host, job.SessionName, exitCode, endTime); err != nil {
			return false, err
		}
		return true, nil
	}

	// No status file - job died unexpectedly
	if err := db.MarkDead(database, job.Host, job.SessionName); err != nil {
		return false, err
	}
	return true, nil
}
