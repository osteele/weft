package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/logcache"
	"github.com/osteele/weft/internal/logfiles"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/ssh"
	"github.com/spf13/cobra"
)

var logCmd = &cobra.Command{
	Use:     "log <job-id>...",
	Aliases: []string{"logs", "output"},
	Short:   "View log output from a job",
	Long: `View the log file for a specific job.

Examples:
  weft log 25           # View log for job #25 (last 50 lines)
  weft log 25 -f        # Follow job #25's log
  weft log 25 -n 100    # Last 100 lines
  weft log 25 --tail 30 # Last 30 lines (alias for -n)
  weft log 25 --from 50           # Lines from 50 onwards
  weft log 25 --from 50 --to 100  # Lines 50-100
  weft log 25 --to 100            # First 100 lines
  weft log 25 --grep error        # Lines containing "error"
  weft log 25 -f --grep epoch     # Follow, filter for "epoch"
  weft log 25 -t 2m               # Use 2 minute SSH timeout (slow connections)

Operations Log (forensic debugging):
  weft log --ops                    # Show recent operations
  weft log --ops --job 1384         # Filter by job ID
  weft log --ops --host cool30      # Filter by host
  weft log --ops --op job.start     # Filter by operation type
  weft log --ops --since 1h         # Operations in last hour
  weft log --ops --errors           # Show only errors`,
	Args: validateLogArgs,
	RunE: runLog,
}

var (
	logFollow  bool
	logLines   int
	logFrom    int
	logTo      int
	logGrep    string
	logFull    bool
	logTimeout time.Duration

	// Operations log flags
	logOps       bool
	logOpsJob    int64
	logOpsHost   string
	logOpsOp     string
	logOpsSince  string
	logOpsErrors bool
)

func init() {
	rootCmd.AddCommand(logCmd)

	// Job log flags
	logCmd.Flags().BoolVarP(&logFollow, "follow", "f", false, "Follow log in real-time")
	logCmd.Flags().IntVarP(&logLines, "lines", "n", 50, "Number of lines to show (last N lines)")
	logCmd.Flags().IntVar(&logLines, "tail", 50, "Number of lines to show (alias for --lines)")
	logCmd.Flags().IntVar(&logFrom, "from", 0, "Show lines starting from line N")
	logCmd.Flags().IntVar(&logTo, "to", 0, "Show lines up to line N")
	logCmd.Flags().StringVar(&logGrep, "grep", "", "Filter lines matching pattern")
	logCmd.Flags().BoolVar(&logFull, "full", false, "Show the entire log (alias for --from 1)")
	logCmd.Flags().DurationVarP(&logTimeout, "timeout", "t", 0, "SSH timeout for slow connections (e.g., 2m, 120s)")

	// Operations log flags
	logCmd.Flags().BoolVar(&logOps, "ops", false, "Show operations log instead of job log")
	logCmd.Flags().Int64Var(&logOpsJob, "job", 0, "Filter operations by job ID (requires --ops)")
	logCmd.Flags().StringVar(&logOpsHost, "host", "", "Filter operations by host (requires --ops)")
	logCmd.Flags().StringVar(&logOpsOp, "op", "", "Filter by operation type (requires --ops)")
	logCmd.Flags().StringVar(&logOpsSince, "since", "", "Show operations since duration (e.g., 1h, 30m) (requires --ops)")
	logCmd.Flags().BoolVar(&logOpsErrors, "errors", false, "Show only operations with errors (requires --ops)")
}

// validateLogArgs validates command arguments based on whether --ops is used
func validateLogArgs(cmd *cobra.Command, args []string) error {
	if logOps {
		// --ops mode doesn't require a job ID argument
		if len(args) > 0 {
			return usageErrorf("--ops does not take a job ID argument")
		}
		return nil
	}
	// Standard mode requires exactly one job ID argument
	return usageArgs(cobra.MinimumNArgs(1))(cmd, args)
}

func runLog(cmd *cobra.Command, args []string) error {
	// Handle --ops mode
	if logOps {
		return runOpsLog(cmd)
	}

	jobIDs, err := ParseJobIDs(args)
	if err != nil {
		return err
	}
	if logFollow && len(jobIDs) > 1 {
		return fmt.Errorf("--follow can only be used with a single job ID")
	}

	if logFull {
		if cmd.Flags().Changed("from") {
			return fmt.Errorf("--full cannot be combined with --from")
		}
		logFrom = 1
	}

	// Validate flag combinations
	hasLineRange := logFrom > 0 || logTo > 0
	if hasLineRange && (cmd.Flags().Changed("lines") || cmd.Flags().Changed("tail")) {
		return fmt.Errorf("--from/--to cannot be used with -n/--lines/--tail")
	}
	if logFollow && logTo > 0 {
		return fmt.Errorf("--follow cannot be used with --to")
	}

	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var errorsList []string
	for i, jobID := range jobIDs {
		if len(jobIDs) > 1 {
			if i > 0 {
				fmt.Println()
			}
			fmt.Printf("Job %d:\n", jobID)
		}
		if err := runLogForJob(cmd, database, jobID); err != nil {
			errorsList = append(errorsList, err.Error())
		}
	}

	if len(errorsList) > 0 {
		return fmt.Errorf("errors: %s", strings.Join(errorsList, "; "))
	}
	return nil
}

func runLogForJob(cmd *cobra.Command, database *sql.DB, jobID int64) error {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return fmt.Errorf("get job: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %d not found", jobID)
	}

	// Cloud jobs: SSH for running, R2 for completed
	if job.IsCloudJob() {
		return runLogForCloudJob(cmd, database, job)
	}

	follow := logFollow
	if follow && isTerminalStatus(job.Status) {
		fmt.Fprintf(os.Stderr, "Job %d already completed; showing log output without following.\n", jobID)
		follow = false
	}

	defaultTailHint := shouldShowDefaultTailHint(cmd, follow)
	tailHintPrinted := false

	// For terminal jobs, prefer local cache first (unless following).
	// Complete caches can serve any request; partial caches only serve default tail views.
	if !follow && shouldPreferCachedLog(job.Status) {
		if cached, err := logcache.Read(jobID); err == nil {
			isComplete := logcache.IsComplete(jobID)
			isDefaultTailView := !logFull && logFrom == 0 && logTo == 0 && !cmd.Flags().Changed("lines") && !cmd.Flags().Changed("tail")
			if isComplete || isDefaultTailView {
				if defaultTailHint && !tailHintPrinted {
					printDefaultTailHint(jobID)
					tailHintPrinted = true
				}
				output := filterLogContent(cached, logFrom, logTo, logLines, logGrep)
				fmt.Print(processCarriageReturns(output))
				return nil
			}
		}
		// Fall through to remote fetch if not cached or partial cache can't serve this request
	}

	// Raise SSH connect timeout to match --timeout so slow connections succeed
	if logTimeout > 0 {
		ssh.SetMinConnectTimeout(logTimeout)
	}

	// Determine log file path using shared resolver
	logFile, resolved := logfiles.ResolveWithTimeout(job, logTimeout)

	// Check if log file exists (skip when resolver already confirmed it)
	exists := resolved
	if follow {
		if !exists {
			if err := waitForLogFile(database, job, logFile); err != nil {
				return err
			}
			exists = true
		}
	} else if !exists {
		exists, err = ssh.RemoteFileExistsWithTimeout(job.Host, logFile, logTimeout)
		if err != nil {
			return fmt.Errorf("check log file: %w", err)
		}
	}
	if !exists {
		return fmt.Errorf("log file not found for job %d on %s", jobID, job.Host)
	}

	// Build the remote command based on flags
	remoteCmd := buildLogCommand(logFile, follow)

	if follow {
		fmt.Printf("\nFollowing log output until job completes (Ctrl+C to stop)...\n\n")
		sshCmd := exec.Command("ssh", job.Host, remoteCmd)
		sshCmd.Stdout = os.Stdout
		sshCmd.Stderr = os.Stderr
		return streamCommandUntilJobDone(database, job.ID, sshCmd)
	}

	// For terminal jobs not yet cached, try to fetch the full file for caching.
	// If the file is small enough, we fetch it entirely and apply filters locally.
	if shouldPreferCachedLog(job.Status) && !logcache.Exists(jobID) {
		if fullContent, cached := fetchAndCacheFullLog(job.Host, logFile, jobID); cached {
			if defaultTailHint && !tailHintPrinted {
				printDefaultTailHint(jobID)
				tailHintPrinted = true
			}
			output := filterLogContent(fullContent, logFrom, logTo, logLines, logGrep)
			fmt.Print(processCarriageReturns(output))
			return nil
		}
		// File too large or fetch failed — fall through to filtered remote command
	}

	// Regular mode: run filtered command on remote
	var stdout, stderr string
	if logTimeout > 0 {
		stdout, stderr, err = ssh.RunWithTimeout(job.Host, remoteCmd, logTimeout)
	} else {
		stdout, stderr, err = ssh.Run(job.Host, remoteCmd)
	}
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			if cached, cacheErr := logcache.Read(jobID); cacheErr == nil {
				fmt.Fprintf(os.Stderr, "Warning: host unreachable; using cached log for job %d.\n", jobID)
				output := filterLogContent(cached, logFrom, logTo, logLines, logGrep)
				fmt.Print(processCarriageReturns(output))
				return nil
			}
		}
		// Provide user-friendly error messages without leaking internal paths
		if strings.Contains(stderr, "No such file") || strings.Contains(stderr, "cannot open") {
			return fmt.Errorf("log file not found for job %d on %s", jobID, job.Host)
		}
		if strings.Contains(stderr, "Permission denied") {
			return fmt.Errorf("permission denied reading log for job %d on %s", jobID, job.Host)
		}
		if stderr != "" {
			return fmt.Errorf("could not read log for job %d on %s: %s", jobID, job.Host, stderr)
		}
		return fmt.Errorf("could not read log for job %d on %s: %w", jobID, job.Host, err)
	}

	// Cache partial output for terminal jobs (not marked complete since it's filtered)
	if shouldPreferCachedLog(job.Status) && !logcache.Exists(jobID) {
		_ = logcache.WriteWithMeta(jobID, stdout, false)
	}

	// Process carriage returns - progress bars use \r to overwrite lines
	if defaultTailHint && !tailHintPrinted {
		printDefaultTailHint(jobID)
		tailHintPrinted = true
	}
	fmt.Print(processCarriageReturns(stdout))
	return nil
}

// runLogForCloudJob fetches log output for a cloud-based job.
// For running jobs, it SSHes into the cloud instance (same as on-prem).
// For terminal jobs, it fetches from R2.
func runLogForCloudJob(cmd *cobra.Command, database *sql.DB, job *db.Job) error {
	// For terminal jobs, prefer local cache first.
	if shouldPreferCachedLog(job.Status) {
		if cached, err := logcache.Read(job.ID); err == nil {
			isComplete := logcache.IsComplete(job.ID)
			isDefaultTailView := !logFull && logFrom == 0 && logTo == 0 && !cmd.Flags().Changed("lines") && !cmd.Flags().Changed("tail")
			if isComplete || isDefaultTailView {
				if shouldShowDefaultTailHint(cmd, false) {
					printDefaultTailHint(job.ID)
				}
				output := filterLogContent(cached, logFrom, logTo, logLines, logGrep)
				fmt.Print(processCarriageReturns(output))
				return nil
			}
		}
	}

	// For running jobs, SSH into the cloud instance to read the log directly
	if !isTerminalStatus(job.Status) {
		inst, err := resolveCloudInstanceSSH(database, job)
		if err == nil {
			return runLogViaCloudSSH(cmd, database, job, inst)
		}
		// Fall through to R2 if we can't resolve SSH details
		fmt.Fprintf(os.Stderr, "Warning: could not resolve cloud instance SSH (%v); trying R2\n", err)
	}

	return runLogFromR2(cmd, job)
}

// resolveCloudInstanceSSH looks up the cloud instance for a job and returns its SSH details.
func resolveCloudInstanceSSH(database *sql.DB, job *db.Job) (*cloud.Instance, error) {
	if job.CloudInstanceID == nil {
		return nil, fmt.Errorf("job has no cloud instance ID")
	}

	ci, err := db.GetCloudInstance(database, *job.CloudInstanceID)
	if err != nil {
		return nil, fmt.Errorf("get cloud instance: %w", err)
	}
	if ci == nil {
		return nil, fmt.Errorf("cloud instance %d not found", *job.CloudInstanceID)
	}

	providerID := ci.EffectiveProviderID()
	if providerID == "" {
		return nil, fmt.Errorf("cloud instance has no provider ID")
	}

	client := cloudClientForDBInstance(ci.Provider)
	inst, err := client.ShowInstance(providerID)
	if err != nil {
		return nil, fmt.Errorf("show instance: %w", err)
	}
	if inst.SSHHost == "" {
		return nil, fmt.Errorf("instance has no SSH host")
	}

	return inst, nil
}

// cloudLogDir is the directory where the agent writes job logs on cloud instances.
const cloudLogDir = "/tmp/weft-logs"

// runLogViaCloudSSH reads a job's log by SSHing into the cloud instance.
func runLogViaCloudSSH(cmd *cobra.Command, database *sql.DB, job *db.Job, inst *cloud.Instance) error {
	logFile := fmt.Sprintf("%s/%d.log", cloudLogDir, job.ID)
	remoteCmd := buildLogCommand(logFile, logFollow)

	target := cloud.InstanceSSHTarget(inst)
	sshOpts := cloud.InstanceSSHArgs(inst)
	sshArgs := append(sshOpts, target, remoteCmd)

	defaultTailHint := shouldShowDefaultTailHint(cmd, logFollow)

	if logFollow {
		fmt.Printf("\nFollowing log output until job completes (Ctrl+C to stop)...\n\n")
		sshCmd := exec.Command("ssh", sshArgs...)
		sshCmd.Stdout = os.Stdout
		sshCmd.Stderr = os.Stderr
		return streamCommandUntilJobDone(database, job.ID, sshCmd)
	}

	out, err := cloud.RunOnInstance(inst, remoteCmd, 30*time.Second)
	if err != nil {
		return fmt.Errorf("ssh to cloud instance: %w", err)
	}

	if defaultTailHint {
		printDefaultTailHint(job.ID)
	}

	output := filterLogContent(out, logFrom, logTo, logLines, logGrep)
	fmt.Print(processCarriageReturns(output))
	return nil
}

// runLogFromR2 fetches log output from R2 for a cloud-based job.
func runLogFromR2(cmd *cobra.Command, job *db.Job) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	r2Client, err := buildR2Client(cfg)
	if err != nil {
		return fmt.Errorf("create R2 client: %w", err)
	}
	if r2Client == nil {
		return fmt.Errorf("R2 not configured; cannot fetch cloud job logs")
	}

	if logFollow {
		fmt.Fprintf(os.Stderr, "Follow mode is not supported for cloud job logs from R2; showing current log.\n")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key := r2keys.JobResultLog(job.ID)
	data, err := r2Client.GetObject(ctx, key)
	if err != nil {
		return fmt.Errorf("fetch log from R2 (key %s): %w", key, err)
	}

	content := string(data)

	// Cache for terminal jobs
	if shouldPreferCachedLog(job.Status) {
		_ = logcache.Write(job.ID, content)
	}

	defaultTailHint := shouldShowDefaultTailHint(cmd, false)
	if defaultTailHint {
		printDefaultTailHint(job.ID)
	}

	output := filterLogContent(content, logFrom, logTo, logLines, logGrep)
	fmt.Print(processCarriageReturns(output))
	return nil
}

func waitForLogFile(database *sql.DB, job *db.Job, logFile string) error {
	warned := false
	for {
		exists, err := ssh.RemoteFileExists(job.Host, logFile)
		if err != nil {
			if ssh.IsConnectionError(err.Error()) {
				if !warned {
					fmt.Fprintf(os.Stderr, "%s is offline; waiting for it to come online...\n", job.Host)
					warned = true
				}
				time.Sleep(1 * time.Second)
				continue
			}
			return fmt.Errorf("check log file: %w", err)
		}
		if exists {
			return nil
		}
		refreshed, err := db.GetJobByID(database, job.ID)
		if err != nil {
			return err
		}
		if refreshed != nil {
			job = refreshed
		}
		if job != nil && isTerminalStatus(job.Status) {
			return fmt.Errorf("log file not found for job %d on %s", job.ID, job.Host)
		}
		time.Sleep(1 * time.Second)
	}
}

func shouldPreferCachedLog(status string) bool {
	switch status {
	case db.StatusCompleted, db.StatusDead, db.StatusFailed, db.StatusKilled, db.StatusCanceled:
		return true
	default:
		return false
	}
}

// fetchAndCacheFullLog attempts to fetch the full log file from remote and cache it.
// Returns (content, true) if the file was fetched and cached, or ("", false) if
// the file is too large or the fetch failed.
func fetchAndCacheFullLog(host, logFile string, jobID int64) (string, bool) {
	cfg, _ := config.Load()
	maxSize := cfg.LogCacheMaxSize
	if maxSize <= 0 {
		return "", false
	}

	// Check file size first
	sizeCmd := fmt.Sprintf("stat -c %%s %s 2>/dev/null || stat -f %%z %s 2>/dev/null", logFile, logFile)
	var sizeOut string
	var err error
	if logTimeout > 0 {
		sizeOut, _, err = ssh.RunWithTimeout(host, sizeCmd, logTimeout)
	} else {
		sizeOut, _, err = ssh.Run(host, sizeCmd)
	}
	if err != nil {
		return "", false
	}

	size, err := strconv.ParseInt(strings.TrimSpace(sizeOut), 10, 64)
	if err != nil {
		return "", false
	}

	if size > int64(maxSize) {
		return "", false
	}

	// Fetch the full file
	catCmd := fmt.Sprintf("cat %s", logFile)
	var content string
	if logTimeout > 0 {
		content, _, err = ssh.RunWithTimeout(host, catCmd, logTimeout)
	} else {
		content, _, err = ssh.Run(host, catCmd)
	}
	if err != nil {
		return "", false
	}

	// Cache as complete
	_ = logcache.Write(jobID, content)
	return content, true
}

// buildLogCommand constructs the remote command for reading log files
// based on the provided flags (--from, --to, -n, --grep, -f)
func buildLogCommand(logFile string, follow bool) string {
	var cmd string

	// Determine line selection strategy
	// Priority: --from/--to > -n (default)
	if logFrom > 0 && logTo > 0 {
		// Lines from N to M: tail -n +N | head -n (M-N+1)
		count := logTo - logFrom + 1
		if count < 1 {
			count = 1
		}
		cmd = fmt.Sprintf("tail -n +%d %s | head -n %d", logFrom, logFile, count)
	} else if logFrom > 0 {
		// Lines from N onwards
		if follow {
			// For follow mode with --from: get from line N then follow
			// Use -F to retry if file doesn't exist yet, suppress errors
			cmd = fmt.Sprintf("tail -n +%d -F %s 2>/dev/null", logFrom, logFile)
		} else {
			cmd = fmt.Sprintf("tail -n +%d %s", logFrom, logFile)
		}
	} else if logTo > 0 {
		// First N lines (up to line N)
		cmd = fmt.Sprintf("head -n %d %s", logTo, logFile)
	} else if follow {
		// Follow mode with default or -n lines
		// Use -F to retry if file doesn't exist yet or gets recreated
		// Suppress "cannot open" errors (file might not exist yet for new jobs)
		cmd = fmt.Sprintf("tail -n %d -F %s 2>/dev/null", logLines, logFile)
	} else {
		// Default: last N lines
		cmd = fmt.Sprintf("tail -n %d %s", logLines, logFile)
	}

	// Add grep filter if specified
	if logGrep != "" {
		if follow {
			// Use --line-buffered for real-time grep output
			cmd = fmt.Sprintf("%s | grep --line-buffered '%s'", cmd, escapeShellArg(logGrep))
		} else {
			cmd = fmt.Sprintf("%s | grep '%s'", cmd, escapeShellArg(logGrep))
		}
	}

	return cmd
}

func shouldShowDefaultTailHint(cmd *cobra.Command, follow bool) bool {
	if follow || logFull {
		return false
	}
	if logFrom > 0 || logTo > 0 {
		return false
	}
	if cmd.Flags().Changed("lines") || cmd.Flags().Changed("tail") || cmd.Flags().Changed("from") || cmd.Flags().Changed("to") {
		return false
	}
	return true
}

func printDefaultTailHint(jobID int64) {
	fmt.Printf("(showing last %d lines; run 'weft log %d --full' to see the entire log or adjust -n/--lines)\n\n", logLines, jobID)
}

func escapeShellArg(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

// filterLogContent applies line range and grep filters to cached log content
func filterLogContent(content string, from, to, lines int, grepPattern string) string {
	allLines := strings.Split(content, "\n")

	// Remove trailing empty line if content ends with newline
	if len(allLines) > 0 && allLines[len(allLines)-1] == "" {
		allLines = allLines[:len(allLines)-1]
	}

	var result []string

	// Apply line range filters
	if from > 0 && to > 0 {
		// Lines from N to M (1-indexed)
		start := from - 1
		end := to
		if start < 0 {
			start = 0
		}
		if end > len(allLines) {
			end = len(allLines)
		}
		if start < len(allLines) {
			result = allLines[start:end]
		}
	} else if from > 0 {
		// Lines from N onwards (1-indexed)
		start := from - 1
		if start < 0 {
			start = 0
		}
		if start < len(allLines) {
			result = allLines[start:]
		}
	} else if to > 0 {
		// First N lines
		end := to
		if end > len(allLines) {
			end = len(allLines)
		}
		result = allLines[:end]
	} else {
		// Default: last N lines
		start := len(allLines) - lines
		if start < 0 {
			start = 0
		}
		result = allLines[start:]
	}

	// Apply grep filter if specified
	if grepPattern != "" {
		re, err := regexp.Compile(grepPattern)
		match := func(line string) bool {
			if err != nil {
				return strings.Contains(line, grepPattern)
			}
			return re.MatchString(line)
		}
		var filtered []string
		for _, line := range result {
			if match(line) {
				filtered = append(filtered, line)
			}
		}
		result = filtered
	}

	if len(result) == 0 {
		return ""
	}
	return strings.Join(result, "\n") + "\n"
}

// processCarriageReturns handles \r characters used by progress bars.
// For each line, it returns only the final segment after the last \r,
// simulating what the terminal would display.
func processCarriageReturns(content string) string {
	lines := strings.Split(content, "\n")
	var result []string

	for _, line := range lines {
		// If line contains \r, take only the part after the last \r
		if idx := strings.LastIndex(line, "\r"); idx >= 0 {
			line = line[idx+1:]
		}
		result = append(result, line)
	}

	return strings.Join(result, "\n")
}

// runOpsLog displays the operations log with optional filtering
func runOpsLog(cmd *cobra.Command) error {
	// Read entries from log file
	logPath := oplog.DefaultLogPath()
	entries, err := oplog.ReadEntries(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No operations log found. Operations logging may be disabled or no operations have been logged yet.")
			return nil
		}
		return fmt.Errorf("read operations log: %w", err)
	}

	if len(entries) == 0 {
		fmt.Println("Operations log is empty.")
		return nil
	}

	// Build filter options
	filterOpts := oplog.FilterOptions{
		JobID:      logOpsJob,
		Host:       logOpsHost,
		Operation:  logOpsOp,
		ErrorsOnly: logOpsErrors,
	}

	// Parse --since duration
	if logOpsSince != "" {
		duration, err := time.ParseDuration(logOpsSince)
		if err != nil {
			return fmt.Errorf("invalid --since duration %q: %w", logOpsSince, err)
		}
		filterOpts.Since = time.Now().Add(-duration)
	}

	// Apply filters
	filtered := oplog.FilterEntries(entries, filterOpts)

	if len(filtered) == 0 {
		fmt.Println("No matching operations found.")
		return nil
	}

	// Display entries
	for _, entry := range filtered {
		formatOpsEntry(entry)
	}

	return nil
}

// formatOpsEntry formats and prints a single operations log entry
func formatOpsEntry(entry oplog.Entry) {
	// Format: TIME OP [job:ID] [host:HOST] [DETAIL] [ERROR]
	timestamp := entry.Time.Local().Format("2006-01-02 15:04:05")

	var parts []string
	parts = append(parts, timestamp)
	parts = append(parts, entry.Operation)

	if entry.JobID != 0 {
		parts = append(parts, fmt.Sprintf("job:%d", entry.JobID))
	}
	if entry.Host != "" {
		parts = append(parts, fmt.Sprintf("host:%s", entry.Host))
	}
	if entry.Detail != "" {
		parts = append(parts, entry.Detail)
	}
	if entry.Duration > 0 {
		parts = append(parts, fmt.Sprintf("(%dms)", entry.Duration))
	}
	if entry.Error != "" {
		parts = append(parts, fmt.Sprintf("ERROR: %s", entry.Error))
	}

	fmt.Println(strings.Join(parts, " "))
}
