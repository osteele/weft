package cmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/osteele/remote-jobs/internal/config"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
	"github.com/osteele/remote-jobs/internal/ops"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run [flags] <host> <command>",
	Short: "Queue a job on a remote host",
	Long: `Queue a job on a remote host for sequential execution.

By default, jobs are added to a queue and run sequentially.
Use --immediate (-i) to start a job immediately instead of adding it to the queue.

Examples:
  remote-jobs run cool30 'python train.py'           # Queue job
  remote-jobs run -i cool30 'python train.py'        # Start immediately
  remote-jobs run -m "Training" cool30 'python train.py'
  remote-jobs run -C /mnt/code/LM2 cool30 'python train.py'
  remote-jobs run --after 42 cool30 'python eval.py' # Run after job 42
  remote-jobs run -i -f cool30 'python train.py'     # Start and follow log`,
	Args: usageArgs(func(cmd *cobra.Command, args []string) error {
		// --kill mode only needs host
		if runKillJobID > 0 {
			if len(args) < 1 {
				return fmt.Errorf("requires host argument")
			}
			return nil
		}
		// Normal mode needs exactly host + command
		if len(args) != 2 {
			return fmt.Errorf("requires <host> <command>")
		}
		return nil
	}),
	RunE: runRun,
}

var (
	runDir         string
	runDescription string
	runImmediate   bool
	runDraft       bool
	runFollow      bool
	runAllow       bool
	runKillJobID   int64
	runFrom        int64
	runTimeout     string
	runEnvVars     []string
	runTags        []string
	runAfter       int64
	runAfterAny    int64
)

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().BoolVarP(&runImmediate, "immediate", "i", false, "Start job immediately instead of queuing")
	runCmd.Flags().BoolVar(&runDraft, "draft", false, "Create the job in draft status without contacting remote hosts")
	runCmd.Flags().StringVarP(&runDir, "directory", "C", "", "Working directory (default: current directory path)")
	runCmd.Flags().StringVarP(&runDescription, "message", "m", "", "Description of the job")
	runCmd.Flags().StringVarP(&runDescription, "description", "d", "", "[deprecated: use -m] Description of the job")
	runCmd.Flags().MarkHidden("description")
	runCmd.Flags().BoolVarP(&runFollow, "follow", "f", false, "Follow log output after starting")
	runCmd.Flags().BoolVar(&runAllow, "allow", false, "Stream the job log live and stay attached until interrupted")
	runCmd.Flags().Int64Var(&runKillJobID, "kill", 0, "Kill a job by ID (synonym for 'remote-jobs kill')")
	runCmd.Flags().Int64Var(&runFrom, "from", 0, "Copy settings from existing job ID before running")
	runCmd.Flags().StringVar(&runTimeout, "timeout", "", "Kill job after duration (e.g., \"2h\", \"30m\", \"1h30m\")")
	runCmd.Flags().StringSliceVarP(&runEnvVars, "env", "e", nil, "Environment variable (VAR=value), can be repeated")
	runCmd.Flags().StringSliceVar(&runTags, "tag", nil, "Tag to attach to the job (can be repeated)")
	runCmd.Flags().Int64Var(&runAfter, "after", 0, "Start job after another job succeeds (implies --queue)")
	runCmd.Flags().Int64Var(&runAfter, "depends-on", 0, "Alias for --after; start job after another job succeeds (implies --queue)")
	runCmd.Flags().Int64Var(&runAfterAny, "after-any", 0, "Start job after another job completes, success or failure (implies --queue)")
}

func runRun(cmd *cobra.Command, args []string) error {
	// Handle --kill mode
	if runKillJobID > 0 {
		oplog.Log(oplog.OpCLICommand, oplog.WithDetail("kill"), oplog.WithJobID(runKillJobID))
		result, err := killJobWithService(nil, runKillJobID, ops.TimeoutNormal)
		if err != nil {
			return err
		}
		if result.Outcome.Message != "" {
			fmt.Println(result.Outcome.Message)
		}
		return nil
	}

	// Open database early for --from support
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var host, command string

	// Handle --from mode: copy settings from existing job
	if runFrom > 0 {
		fromJob, err := db.GetJobByID(database, runFrom)
		if err != nil {
			return fmt.Errorf("get job %d: %w", runFrom, err)
		}
		if fromJob == nil {
			return fmt.Errorf("job %d not found", runFrom)
		}

		// Copy settings from existing job
		host = fromJob.Host
		command = fromJob.Command
		if runDir == "" {
			runDir = fromJob.WorkingDir
		}
		if runDescription == "" {
			runDescription = fromJob.Description
		}
		if len(runTags) == 0 {
			runTags = append([]string(nil), fromJob.Tags...)
		}

		// Allow overriding host from command line
		if len(args) > 0 {
			host = args[0]
		}
		// Allow overriding command from command line
		if len(args) > 1 {
			command = args[1]
		}
	} else {
		// Normal mode: require host and command
		if len(args) < 2 {
			return usageErrorf("usage: remote-jobs run <host> <command>")
		}
		host = args[0]
		command = args[1]
	}

	// Validate command against blocked patterns
	cfg, _ := config.Load()
	setUsageHintsFromConfig(cfg)
	if err := cfg.ValidateCommand(command); err != nil {
		return err
	}

	// Print recommendations for common patterns
	printCommandRecommendations(command)

	// Validate flag combinations
	// --follow and --allow require --immediate (can't follow a queued job)
	if runFollow && !runImmediate {
		return fmt.Errorf("--follow requires --immediate (-i) since jobs are queued by default")
	}
	if runAllow && !runImmediate {
		return fmt.Errorf("--allow requires --immediate (-i) since jobs are queued by default")
	}
	if runFollow && runAfter > 0 {
		return fmt.Errorf("--follow cannot be used with --after/--depends-on")
	}
	if runAllow && runAfter > 0 {
		return fmt.Errorf("--allow cannot be used with --after/--depends-on")
	}
	if runFollow && runAfterAny > 0 {
		return fmt.Errorf("--follow cannot be used with --after-any")
	}
	if runAllow && runAfterAny > 0 {
		return fmt.Errorf("--allow cannot be used with --after-any")
	}
	if runAfter > 0 && runAfterAny > 0 {
		return fmt.Errorf("cannot use both --after/--depends-on and --after-any")
	}
	if runAllow && runFollow {
		return fmt.Errorf("--allow cannot be used with --follow")
	}
	if runImmediate && (runAfter > 0 || runAfterAny > 0) {
		return fmt.Errorf("--immediate cannot be used with --after/--depends-on or --after-any")
	}
	if runDraft && runImmediate {
		return fmt.Errorf("--draft cannot be combined with --immediate")
	}
	if runDraft && (runFollow || runAllow) {
		return fmt.Errorf("--draft cannot be combined with --follow/--allow")
	}
	if runDraft && (runAfter > 0 || runAfterAny > 0) {
		return fmt.Errorf("--draft cannot be combined with --after/--depends-on or --after-any")
	}

	dirProvided := runDir != ""

	// Parse "cd /path && command" pattern to extract working directory
	// Only if -C/--directory wasn't explicitly provided
	parsedDir, parsedCmd := parseCdPrefix(command)
	if parsedDir != "" && runDir == "" {
		command = parsedCmd
		runDir = parsedDir
		dirProvided = true
		displayCdRewriteMessage(cmd, host, runDir, command)
	}

	// Set defaults
	workingDir := runDir
	if workingDir == "" {
		var err error
		workingDir, err = session.DefaultWorkingDir()
		if err != nil {
			return fmt.Errorf("get working dir: %w", err)
		}
	}

	if dirProvided {
		maybeWarnHomePrefixedDir(host, workingDir)
	}

	// Log CLI command invocation
	mode := "queue"
	if runImmediate {
		mode = "immediate"
	}
	oplog.Log(oplog.OpCLICommand, oplog.WithHost(host), oplog.WithDetailf("run mode=%s cmd=%s", mode, command))

	if runDraft {
		gpu := extractGPUFromEnvVars(runEnvVars)
		jobID, err := db.RecordDraftJobWithGPU(database, host, workingDir, command, runDescription, defaultQueueName, gpu)
		if err != nil {
			return fmt.Errorf("record draft job: %w", err)
		}
		if err := db.SetJobTags(database, jobID, runTags); err != nil {
			return fmt.Errorf("set job tags: %w", err)
		}
		fmt.Printf("Draft job #%d saved for %s\n\n", jobID, host)
		fmt.Printf("  Working dir: %s\n", workingDir)
		fmt.Printf("  Command: %s\n", command)
		if runDescription != "" {
			fmt.Printf("  Description: %s\n", runDescription)
		}
		return nil
	}

	// Handle --after/--depends-on and --after-any dependencies (always uses remote queue)
	if runAfter > 0 || runAfterAny > 0 {
		deps := []queueDependency{}
		waitType := "succeeds"
		afterID := runAfter
		if runAfterAny > 0 {
			afterID = runAfterAny
			waitType = "completes"
			if err := ensureSameHostDependency(database, afterID, host); err != nil {
				return err
			}
			deps = append(deps, queueDependency{JobID: afterID, AllowFailure: true})
		} else if runAfter > 0 {
			if err := ensureSameHostDependency(database, afterID, host); err != nil {
				return err
			}
			deps = append(deps, queueDependency{JobID: afterID, AllowFailure: false})
		}
		res, err := queueJob(database, queueJobOptions{
			Host:         host,
			WorkingDir:   workingDir,
			Command:      command,
			Description:  runDescription,
			EnvVars:      runEnvVars,
			Tags:         runTags,
			QueueName:    defaultQueueName,
			Dependencies: deps,
			AutoStart:    true,
		})
		if err != nil {
			return fmt.Errorf("queue job: %w", err)
		}
		jobID := res.JobID
		fmt.Printf("Job %d added to queue on %s, will run after job %d %s\n\n", jobID, host, afterID, waitType)
		fmt.Printf("  Working dir: %s\n", workingDir)
		fmt.Printf("  Command: %s\n", command)
		if runDescription != "" {
			fmt.Printf("  Description: %s\n", runDescription)
		}
		if len(runEnvVars) > 0 {
			fmt.Printf("  Env vars: %s\n", strings.Join(runEnvVars, ", "))
		}
		fmt.Printf("  After job: %d (%s)\n", afterID, waitType)

		if res.Deferred {
			fmt.Printf("\nHost %s is unreachable right now. The CLI will append this job to the remote queue once it can reach the host again (run `remote-jobs sync --sync` to retry).\n", host)
		}
		return nil
	}

	// Default behavior: queue job for sequential execution (unless --immediate)
	if !runImmediate {
		res, err := queueJob(database, queueJobOptions{
			Host:        host,
			WorkingDir:  workingDir,
			Command:     command,
			Description: runDescription,
			EnvVars:     runEnvVars,
			Tags:        runTags,
			QueueName:   defaultQueueName,
			AutoStart:   true,
		})
		if err != nil {
			return fmt.Errorf("queue job: %w", err)
		}
		jobID := res.JobID
		fmt.Printf("Job #%d queued on %s\n\n", jobID, host)
		fmt.Printf("  Working dir: %s\n", workingDir)
		fmt.Printf("  Command: %s\n", command)
		if runDescription != "" {
			fmt.Printf("  Description: %s\n", runDescription)
		}
		if len(runEnvVars) > 0 {
			fmt.Printf("  Env vars: %s\n", strings.Join(runEnvVars, ", "))
		}
		if res.Deferred {
			fmt.Printf("\nHost %s is unreachable. Job will be queued when host becomes available.\n", host)
		} else {
			// Auto-start queue runner (silently ignore offline errors)
			_, _ = ensureQueueRunnerStarted(host, defaultQueueName)
		}
		return nil
	}

	// --immediate mode: start job now in its own tmux session
	result, err := startJob(database, startJobOptions{
		Host:        host,
		WorkingDir:  workingDir,
		Command:     command,
		Description: runDescription,
		EnvVars:     runEnvVars,
		Tags:        runTags,
		Timeout:     runTimeout,
		OnPrepared: func(info StartJobPreparedInfo) {
			fmt.Printf("Starting job %d on %s\n", info.JobID, info.Host)
			fmt.Printf("Working directory: %s\n", info.WorkingDir)
			fmt.Printf("Command: %s\n", info.Command)
			if info.Description != "" {
				fmt.Printf("Description: %s\n", info.Description)
			}
			fmt.Println()
		},
	})
	if err != nil {
		return err
	}

	if result.DeferredToQueue {
		fmt.Println("SSH connection failed. Job will be added to the remote queue on the next sync.")
		fmt.Printf("Job ID: %d on %s\n", result.Info.JobID, result.Info.Host)
		fmt.Printf("Run 'remote-jobs sync %s' when the host is reachable to trigger the start.\n", result.Info.Host)
		return nil
	}

	if result.SlackEnabled {
		fmt.Println("Slack notifications: enabled")
	}

	fmt.Println("✓ Session started successfully")
	fmt.Printf("Job ID: %d\n", result.Info.JobID)

	if runAllow {
		return streamJobLogAllow(host, result.Info.LogFile, result.Info.JobID)
	}

	if runFollow {
		fmt.Printf("\nFollowing log output until job completes (Ctrl+C to stop)...\n\n")
		script := fmt.Sprintf("while [ ! -f %s ]; do sleep 1; done; tail -n 50 -F %s", shellQuote(result.Info.LogFile), shellQuote(result.Info.LogFile))
		remoteCmd := fmt.Sprintf("sh -c %s", shellQuote(script))
		sshCmd := exec.Command("ssh", host, remoteCmd)
		sshCmd.Stdout = os.Stdout
		sshCmd.Stderr = os.Stderr
		if err := streamCommandUntilJobDone(database, result.Info.JobID, sshCmd); err != nil {
			return err
		}
		return nil
	}

	if usageHintsEnabled() {
		fmt.Printf("\nMonitor progress:\n")
		fmt.Printf("  remote-jobs status %d                   # Check status\n", result.Info.JobID)
		fmt.Printf("  remote-jobs status --wait %d            # Wait for completion\n", result.Info.JobID)
		fmt.Printf("  remote-jobs status --wait --wait-timeout 30m %d  # Wait with timeout\n", result.Info.JobID)
		fmt.Printf("\nView log:\n")
		fmt.Printf("  remote-jobs log %d                      # View log\n", result.Info.JobID)
		fmt.Printf("  remote-jobs log %d -f                   # Follow log\n", result.Info.JobID)
	}

	return nil
}

// killJob kills a job by ID (used by --kill flag)

// parseCdPrefix extracts "cd /path && " or "cd /path; " prefix from a command.
// Returns (directory, remaining_command) if found, or ("", original_command) if not.
func parseCdPrefix(command string) (dir string, remaining string) {
	// Match: cd <path> && <rest> or cd <path>; <rest>
	// Path can be quoted or unquoted, may contain ~
	trimmed := strings.TrimSpace(command)

	if !strings.HasPrefix(trimmed, "cd ") {
		return "", command
	}

	// Skip "cd "
	rest := trimmed[3:]

	// Find the path - handle quoted and unquoted paths
	var path string
	var afterPath string

	if strings.HasPrefix(rest, "'") {
		// Single-quoted path
		endQuote := strings.Index(rest[1:], "'")
		if endQuote == -1 {
			return "", command
		}
		path = rest[1 : endQuote+1]
		afterPath = rest[endQuote+2:]
	} else if strings.HasPrefix(rest, "\"") {
		// Double-quoted path
		endQuote := strings.Index(rest[1:], "\"")
		if endQuote == -1 {
			return "", command
		}
		path = rest[1 : endQuote+1]
		afterPath = rest[endQuote+2:]
	} else {
		// Unquoted path - ends at space, &&, or ;
		for i, c := range rest {
			if c == ' ' || c == '&' || c == ';' {
				path = rest[:i]
				afterPath = rest[i:]
				break
			}
		}
		if path == "" {
			// No separator found - just "cd path" with no command after
			return "", command
		}
	}

	// Now look for && or ; separator
	afterPath = strings.TrimSpace(afterPath)
	if strings.HasPrefix(afterPath, "&&") {
		remaining = strings.TrimSpace(afterPath[2:])
		return path, remaining
	} else if strings.HasPrefix(afterPath, ";") {
		remaining = strings.TrimSpace(afterPath[1:])
		return path, remaining
	}

	// No valid separator found
	return "", command
}

func displayCdRewriteMessage(cmd *cobra.Command, host, dir, command string) {
	if !usageHintsEnabled() {
		return
	}
	rendered := formatRewrittenCommand(cmd, dir, host, command)
	if rendered == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "\nDetected \"cd %s &&\" in the command. The job will run as though you had invoked:\n  %s\n", dir, rendered)
}

func formatRewrittenCommand(cmd *cobra.Command, dir, host, command string) string {
	if len(os.Args) == 0 {
		return fmt.Sprintf("%s -C %s %s %q", cmd.CommandPath(), dir, host, command)
	}
	nonFlags := cmd.Flags().Args()
	flagEnd := len(os.Args)
	if len(nonFlags) > 0 && flagEnd >= len(nonFlags) {
		flagEnd = len(os.Args) - len(nonFlags)
	}
	if flagEnd < 1 {
		flagEnd = len(os.Args)
	}
	pieces := make([]string, 0, flagEnd+4)
	pieces = append(pieces, os.Args[0])
	if flagEnd > 1 {
		pieces = append(pieces, os.Args[1:flagEnd]...)
	}
	pieces = append(pieces, "-C", dir, host, command)
	for i, part := range pieces {
		pieces[i] = shellQuote(part)
	}
	return strings.Join(pieces, " ")
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.ContainsAny(s, " \t\n\"'`$\\") {
		return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
	}
	return s
}

func maybeWarnHomePrefixedDir(host, dir string) {
	if dir == "" {
		return
	}
	if !usageHintsEnabled() {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	if !pathHasHomePrefix(dir, home) {
		return
	}
	fmt.Fprintf(os.Stderr, "\nWarning: working directory %s will be used literally on %s.\n", dir, host)
	fmt.Fprintf(os.Stderr, "If you intended the remote home directory, quote it instead, e.g. -C '~/path/to/dir'.\n")
}

func pathHasHomePrefix(dir, home string) bool {
	dirClean := filepath.Clean(dir)
	homeClean := filepath.Clean(home)
	if dirClean == homeClean {
		return true
	}
	if strings.HasPrefix(dirClean, homeClean+string(os.PathSeparator)) {
		return true
	}
	return false
}

func streamJobLogAllow(host, logFile string, jobID int64) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Printf("\nFollowing live output (Ctrl+C to stop streaming; job keeps running)...\n\n")
	waitAndTail := fmt.Sprintf("sh -c 'while [ ! -f %s ]; do sleep 1; done; tail -n +1 -F %s'", logFile, logFile)
	sshCmd := exec.CommandContext(ctx, "ssh", host, waitAndTail)
	sshCmd.Stdout = os.Stdout
	sshCmd.Stderr = os.Stderr
	sshCmd.Stdin = nil

	err := sshCmd.Run()
	if ctx.Err() != nil {
		fmt.Printf("\nDetached from log stream.\n")
		printDetachedInstructions(jobID)
		return nil
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nLog streaming stopped with error: %v\n", err)
		printDetachedInstructions(jobID)
		return err
	}

	fmt.Printf("\nLog streaming finished.\n")
	printDetachedInstructions(jobID)
	return nil
}

func printDetachedInstructions(jobID int64) {
	fmt.Printf("Job %d continues running.\n", jobID)
	if usageHintsEnabled() {
		fmt.Printf("View logs later: remote-jobs log %d -f\n", jobID)
		fmt.Printf("Check status:   remote-jobs job status %d\n", jobID)
	}
}

// printCommandRecommendations checks for common command patterns and suggests
// better alternatives using CLI flags. Returns true if any recommendations were printed.
func printCommandRecommendations(command string) bool {
	if !usageHintsEnabled() {
		return false
	}
	var recommendations []string

	// Check for "cd /path && " prefix
	trimmed := strings.TrimSpace(command)
	if strings.HasPrefix(trimmed, "cd ") {
		// Extract the directory for the recommendation
		dir, _ := parseCdPrefix(command)
		if dir != "" {
			recommendations = append(recommendations,
				fmt.Sprintf("Tip: Instead of 'cd %s && ...', consider using -C %q to set the working directory.\n"+
					"     This ensures ~ is expanded on the remote host, not locally.", dir, dir))
		}
	}

	// Check for "VAR=value " prefix (environment variable)
	if idx := strings.Index(trimmed, "="); idx > 0 && idx < len(trimmed)-1 {
		// Check if it looks like VAR=value at the start (VAR must be valid identifier)
		prefix := trimmed[:idx]
		// Valid env var names: start with letter or _, followed by letters, digits, or _
		isValidEnvVar := true
		for i, c := range prefix {
			if i == 0 {
				if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_') {
					isValidEnvVar = false
					break
				}
			} else {
				if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
					isValidEnvVar = false
					break
				}
			}
		}
		if isValidEnvVar {
			// Find the value (up to the next space)
			rest := trimmed[idx+1:]
			var value string
			if spaceIdx := strings.Index(rest, " "); spaceIdx > 0 {
				value = rest[:spaceIdx]
				recommendations = append(recommendations,
					fmt.Sprintf("Tip: Instead of '%s=%s ...', consider using -e %s=%s to set environment variables.", prefix, value, prefix, value))
			}
		}
	}

	if len(recommendations) > 0 {
		fmt.Fprintln(os.Stderr)
		for _, rec := range recommendations {
			fmt.Fprintln(os.Stderr, rec)
		}
		return true
	}
	return false
}
