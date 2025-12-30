package cmd

import (
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/config"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/logcache"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/ssh"
	"github.com/spf13/cobra"
)

var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Sync job statuses from all remote hosts",
	Long: `Sync job statuses by checking all hosts with running jobs.

Automatically finds hosts with running jobs and updates their status
in the local database. Also starts queue runners on hosts with queued jobs.
Connection failures are silently ignored.

Examples:
  remote-jobs sync                    # Sync all hosts
  remote-jobs sync --verbose          # Show progress
  remote-jobs sync --no-queue-start   # Don't start queue runners`,
	RunE: runSync,
}

var (
	syncVerbose      bool
	syncNoQueueStart bool
)

var (
	syncJobFunc            = ops.SyncJob
	syncJobQuickFunc       = ops.SyncJobQuick
	executeDeferredOpsFunc = ops.ExecuteAllDeferredOperations
)

const (
	// FastSyncTimeout is used for --fast mode in list/status commands
	FastSyncTimeout = 2 * time.Second
	// DefaultSyncTimeout is used for default syncs in status commands
	DefaultSyncTimeout = 5 * time.Second
	// NormalSyncTimeout is used for explicit sync commands
	NormalSyncTimeout = 30 * time.Second
)

func init() {
	rootCmd.AddCommand(syncCmd)
	syncCmd.Flags().BoolVarP(&syncVerbose, "verbose", "v", false, "Show detailed progress")
	syncCmd.Flags().BoolVar(&syncNoQueueStart, "no-queue-start", false, "Don't auto-start queue runners")
}

func runSync(cmd *cobra.Command, args []string) error {
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	// Get all unique hosts with running or queued jobs
	hosts, err := db.ListUniqueActiveHosts(database)
	if err != nil {
		return fmt.Errorf("list hosts: %w", err)
	}

	if len(hosts) == 0 {
		fmt.Println("No active jobs to sync")
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

	// Start queue runners on hosts with queued jobs (unless --no-queue-start)
	if !syncNoQueueStart {
		startQueueRunnersForQueuedHosts(database)
	}

	// Prune old cached log files
	cfg, _ := config.Load()
	if cfg.LogCacheMaxAge > 0 {
		maxAge := time.Duration(cfg.LogCacheMaxAge) * 24 * time.Hour
		if pruned, err := logcache.Prune(maxAge); err == nil && pruned > 0 && syncVerbose {
			fmt.Printf("Pruned %d old cached log file(s)\n", pruned)
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

// syncHost syncs all active jobs (running and queued) for a host and returns the count of updated jobs
func syncHost(database *sql.DB, host string) (int, error) {
	// Execute deferred operations FIRST so that queued jobs are added to remote queue
	// before we check their status (prevents marking as dead jobs that are just pending sync)
	if err := executeDeferredOperations(database, host, NormalSyncTimeout); err != nil {
		// Don't fail the sync if deferred operations fail
		if syncVerbose {
			fmt.Fprintf(os.Stderr, "Warning: failed to execute deferred operations for %s: %v\n", host, err)
		}
	}

	jobs, err := db.ListActiveJobs(database, host)
	if err != nil {
		return 0, err
	}

	syncOpts := ops.DefaultSyncOptions()
	var updated int
	for _, job := range jobs {
		changed, err := syncJobFunc(database, job, syncOpts)
		if err != nil {
			return updated, err
		}
		if changed {
			updated++
		}
	}

	return updated, nil
}

// executeDeferredOperations executes pending operations for a host using the unified ops package
func executeDeferredOperations(database *sql.DB, host string, timeout time.Duration) error {
	// Check count first for verbose output
	operations, err := db.GetDeferredOperations(database, host)
	if err != nil {
		return fmt.Errorf("get deferred operations: %w", err)
	}

	if len(operations) == 0 {
		return nil
	}

	if syncVerbose {
		fmt.Printf("  %s: executing %d deferred operation(s)\n", host, len(operations))
	}

	// Use ops package for unified operation execution
	if timeout <= 0 {
		timeout = NormalSyncTimeout
	}

	result, err := executeDeferredOpsFunc(database, host, ops.ExecuteOptions{
		Timeout: timeout,
		Verbose: syncVerbose,
	})
	if err != nil {
		return err
	}

	// Report any errors
	for _, errMsg := range result.Errors {
		if syncVerbose {
			fmt.Fprintf(os.Stderr, "    Warning: %s\n", errMsg)
		}
	}

	return nil
}

type queueAppendError struct {
	op     string
	stderr string
	err    error
}

func (e *queueAppendError) Error() string {
	if e == nil {
		return ""
	}
	msg := strings.TrimSpace(e.stderr)
	if msg == "" && e.err != nil {
		msg = e.err.Error()
	}
	if msg == "" {
		msg = "unknown error"
	}
	return fmt.Sprintf("%s: %s", e.op, msg)
}

func (e *queueAppendError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *queueAppendError) ConnectionError() bool {
	if e == nil {
		return false
	}
	if e.stderr != "" && ssh.IsConnectionError(e.stderr) {
		return true
	}
	if e.err != nil && ssh.IsConnectionError(e.err.Error()) {
		return true
	}
	return false
}

func appendQueueEntry(host, queueName string, jobID int64, workingDir, command, description string, envVars []string, depSpec string) error {
	if workingDir == "" || command == "" {
		return fmt.Errorf("job %d missing command or working dir", jobID)
	}
	if queueName == "" {
		queueName = defaultQueueName
	}

	if _, stderr, err := ssh.Run(host, fmt.Sprintf("mkdir -p %s", queueDir)); err != nil {
		return &queueAppendError{op: "create queue dir", stderr: strings.TrimSpace(stderr), err: err}
	}

	queueFile := fmt.Sprintf("%s/%s.queue", queueDir, queueName)
	lockFile := queueFile + ".lock"

	envVarsB64 := ""
	if len(envVars) > 0 {
		envVarsB64 = base64.StdEncoding.EncodeToString([]byte(strings.Join(envVars, "\n")))
	}

	jobLine := fmt.Sprintf("%d\t%s\t%s\t%s\t%s\t%s", jobID, workingDir, command, description, envVarsB64, depSpec)
	// Use flock to prevent race with queue runner's pop operation
	appendCmd := fmt.Sprintf("flock %s bash -c \"sed -i '/^%d\\t/d' %s 2>/dev/null || true; echo '%s' >> %s\"",
		lockFile, jobID, queueFile, ssh.EscapeForSingleQuotes(jobLine), queueFile)
	if _, stderr, err := ssh.Run(host, appendCmd); err != nil {
		return &queueAppendError{op: "append queue entry", stderr: strings.TrimSpace(stderr), err: err}
	}

	return nil
}

// performSyncWithTimeout performs a sync with specified timeout for list/status commands
// Returns true if sync completed, false if timed out
func performSyncWithTimeout(database *sql.DB, timeout time.Duration, verbose bool) bool {
	hosts, err := db.ListUniqueActiveHosts(database)
	if err != nil || len(hosts) == 0 {
		return true
	}

	// Set timeout for SSH operations
	// We'll use goroutines with a timeout context
	allCompleted := true
	for _, host := range hosts {
		// Try quick sync, but don't wait if it times out
		done := make(chan bool, 1)
		go func(h string) {
			_, err := syncHostWithTimeout(database, h, timeout)
			done <- (err == nil)
		}(host)

		select {
		case <-done:
			// Sync completed
		case <-time.After(timeout):
			// Timed out
			allCompleted = false
		}
	}

	return allCompleted
}

// performFastSync performs a quick sync with fast timeout for list/status commands
// Returns true if sync completed, false if timed out
func performFastSync(database *sql.DB, verbose bool) bool {
	return performSyncWithTimeout(database, FastSyncTimeout, verbose)
}

// syncHostWithTimeout syncs a host with a specific timeout
func syncHostWithTimeout(database *sql.DB, host string, timeout time.Duration) (int, error) {
	// This is a simplified version of syncHost that uses quick timeouts
	if err := executeDeferredOperations(database, host, timeout); err != nil && syncVerbose {
		fmt.Fprintf(os.Stderr, "Warning: failed to execute deferred operations for %s: %v\n", host, err)
	}

	jobs, err := db.ListActiveJobs(database, host)
	if err != nil {
		return 0, err
	}

	syncOpts := ops.SyncOptions{Timeout: timeout}
	var updated int
	for _, job := range jobs {
		// Use quick check with timeout
		changed, err := syncJobQuickFunc(database, job, syncOpts)
		if err != nil {
			return updated, err
		}
		if changed {
			updated++
		}
	}

	return updated, nil
}
