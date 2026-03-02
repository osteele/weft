package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/coordinator"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/intent"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/predictor"
	"github.com/osteele/weft/internal/session"
	"github.com/osteele/weft/internal/ssh"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run [flags] <command>",
	Short: "Queue a job on a remote host",
	Long: `Queue a job on a remote host for sequential execution.

If --host is omitted, automatic placement selects the best host based on
GPU constraints (--gpu-class, --gpu-mem) and data locality (--input).

By default, jobs are added to a queue and run sequentially.
Use --immediate (-i) to start a job immediately instead of adding it to the queue.

Examples:
  weft run 'python train.py'                           # Auto-place on best host
  weft run --gpu-class a100 'python train.py'           # Place on A100 host
  weft run --host cool30 'python train.py'              # Explicit host
  weft run -m "Training" --host cool30 'python train.py'
  weft run --after 42 'python eval.py'                  # Run after job 42
  weft run --wait --host cool30 'python train.py'       # Queue and wait for completion
  weft run -f --host cool30 'python train.py'           # Queue and follow log output
  weft run -i --host cool30 'python train.py'           # Start immediately and follow log`,
	Args: usageArgs(func(cmd *cobra.Command, args []string) error {
		// --kill mode: no positional args needed (host is looked up from the job)
		if runKillJobID > 0 {
			return nil
		}
		// --from mode: 0 args (copies from source job) or 1 arg (command override)
		if runFrom > 0 {
			if len(args) > 1 {
				return fmt.Errorf("--from accepts at most one positional argument (command override)")
			}
			return nil
		}
		// Normal mode: 1 arg (command), or 2 args (deprecated positional host + command)
		if len(args) < 1 || len(args) > 2 {
			return fmt.Errorf("requires <command> argument")
		}
		return nil
	}),
	RunE: runRun,
}

var (
	runHost        string
	runDir         string
	runDescription string
	runImmediate   bool
	runDraft       bool
	runFollow      bool
	runAllow       bool
	runWait        bool
	runNoWait      bool // explicit no-op flag for tooling compatibility
	runKillJobID   int64
	runFrom        int64
	runTimeout     string
	runEnvVars     []string
	runTags        []string
	runAfter       int64
	runAfterAny    int64
	runGPUMem      int
	runGPUClass    string
	runInputs      []string
	runOutputs     []string
	runDryRun      bool
	runNoSync      bool
)

const defaultGPUMemGB = ops.DefaultGPUMemGB

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().StringVarP(&runHost, "host", "H", "", "Remote host to run on (default: auto-place)")
	runCmd.Flags().BoolVarP(&runImmediate, "immediate", "i", false, "Start job immediately instead of queuing")
	runCmd.Flags().BoolVar(&runDraft, "draft", false, "Create the job in draft status without contacting remote hosts")
	runCmd.Flags().StringVarP(&runDir, "directory", "C", "", "Working directory (default: current directory path)")
	runCmd.Flags().StringVarP(&runDescription, "message", "m", "", "Description of the job")
	runCmd.Flags().StringVarP(&runDescription, "description", "d", "", "[deprecated: use -m] Description of the job")
	runCmd.Flags().MarkHidden("description")
	runCmd.Flags().BoolVarP(&runFollow, "follow", "f", false, "Follow log output after starting")
	runCmd.Flags().BoolVar(&runAllow, "allow", false, "Stream the job log live and stay attached until interrupted")
	runCmd.Flags().Int64Var(&runKillJobID, "kill", 0, "Kill a job by ID (synonym for 'weft kill')")
	runCmd.Flags().Int64Var(&runFrom, "from", 0, "Copy settings from existing job ID before running")
	runCmd.Flags().StringVar(&runTimeout, "timeout", "", "Kill job after duration (e.g., \"2h\", \"30m\", \"1h30m\")")
	runCmd.Flags().StringSliceVarP(&runEnvVars, "env", "e", nil, "Environment variable (VAR=value), can be repeated")
	runCmd.Flags().StringSliceVar(&runTags, "tag", nil, "Tag to attach to the job (can be repeated)")
	runCmd.Flags().Int64Var(&runAfter, "after", 0, "Start job after another job succeeds (implies --queue)")
	runCmd.Flags().Int64Var(&runAfter, "depends-on", 0, "Alias for --after; start job after another job succeeds (implies --queue)")
	runCmd.Flags().Int64Var(&runAfterAny, "after-any", 0, "Start job after another job completes, success or failure (implies --queue)")
	runCmd.Flags().IntVar(&runGPUMem, "gpu-mem", 0, "GPU memory reservation in GB per device (default: 20 when GPU is used)")
	runCmd.Flags().StringVar(&runGPUClass, "gpu-class", "", "GPU class or generation (e.g., a100, ampere, ampere+); '+' means that generation or newer")
	runCmd.Flags().BoolVar(&runWait, "wait", false, "Wait for job to complete before returning")
	runCmd.Flags().BoolVar(&runNoWait, "no-wait", false, "Don't wait for job (default behavior, for explicit acknowledgment)")
	runCmd.Flags().StringSliceVar(&runInputs, "input", nil, "Input data asset (e.g., hf:meta-llama/Llama-3-8B), can be repeated")
	runCmd.Flags().StringSliceVar(&runOutputs, "output", nil, "Output data asset (e.g., checkpoint:llama-ft-v1), can be repeated")
	runCmd.Flags().BoolVar(&runDryRun, "dry-run", false, "Show placement scores without submitting the job")
	runCmd.Flags().BoolVar(&runNoSync, "no-sync", false, "Skip source sync before submission")
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

	// --host flag takes priority
	host = runHost

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
		if host == "" {
			host = fromJob.Host
		}
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

		// Allow overriding command from positional arg
		if len(args) > 0 {
			command = args[0]
		}
	} else {
		// Parse positional args
		if len(args) == 2 {
			// Backward compat: 2 args where first looks like a known host
			if inventory.FindHost(args[0]) != nil {
				if host == "" {
					host = args[0]
					fmt.Fprintf(cmd.ErrOrStderr(), "Deprecation: positional host is deprecated. Use: weft run --host %s '%s'\n", args[0], args[1])
				}
				command = args[1]
			} else {
				return usageErrorf("unexpected argument %q (use --host to specify a host)", args[0])
			}
		} else if len(args) == 1 {
			// Single arg: is it a known host name, or a command?
			if inventory.FindHost(args[0]) != nil {
				return usageErrorf("'%s' looks like a host name. Usage: weft run --host %s <command>", args[0], args[0])
			}
			command = args[0]
			// host will be resolved via placement below
		}
	}

	// Validate command against blocked patterns
	cfg, _ := config.Load()
	setUsageHintsFromConfig(cfg)
	if err := cfg.ValidateCommand(command); err != nil {
		return err
	}

	// Load output directories from .weft.yaml for convention-based output collection
	outputDirs := config.ProjectOutputDirs(resolveLocalDir(runDir))

	// Print recommendations for common patterns
	printCommandRecommendations(command)

	// Validate flag combinations
	// --allow requires --immediate (can't use allow mode with a queued job)
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
	if runWait && runFollow {
		return fmt.Errorf("--wait cannot be used with --follow")
	}
	if runWait && runAllow {
		return fmt.Errorf("--wait cannot be used with --allow")
	}
	if runWait && runDraft {
		return fmt.Errorf("--wait cannot be combined with --draft")
	}
	if runWait && runNoWait {
		return fmt.Errorf("--wait and --no-wait cannot be used together")
	}

	// Placement scoring (used for auto-placement and dry-run)
	placementConstraints := placement.Constraints{
		GPUClass: runGPUClass,
		GPUMemGB: runGPUMem,
		Inputs:   runInputs,
		Command:  command,
		Project:  projectFromDir(runDir),
	}

	// Build predictor closure if configured
	predict := buildJobPredictor(cfg, placementConstraints)

	if runDryRun {
		scores, err := placement.ScoreHostsWithPredictor(database, placementConstraints, nil, predict)
		if err != nil {
			return fmt.Errorf("placement scoring: %w", err)
		}

		// Probe all scored hosts for liveness
		allHosts := make([]string, len(scores))
		for i, s := range scores {
			allHosts[i] = s.Host
		}
		liveness := placement.ProbeHosts(allHosts, 5*time.Second)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintf(w, "HOST\tSCORE\tELIGIBLE\tONLINE\tREASONS\n")
		for _, s := range scores {
			eligible := "yes"
			if !s.Eligible {
				eligible = "no"
			}
			online := "yes"
			if !liveness[s.Host] {
				online = "no"
			}
			fmt.Fprintf(w, "%s\t%.1f\t%s\t%s\t%s\n", s.Host, s.Total, eligible, online, strings.Join(s.Reasons, "; "))
		}
		w.Flush()
		return nil
	}

	// Route through coordinator for all non-immediate, non-draft submissions
	if !runImmediate && !runDraft && coordinatorReachable(cfg) {
		return submitIntent(cfg, database, host, command, runDir, runDescription, runEnvVars, runTags, runInputs, runOutputs, runGPUClass, runGPUMem, runAfter)
	}

	// Coordinator unreachable — fall back to direct submission
	if host == "" {
		if runImmediate {
			return fmt.Errorf("--immediate requires an explicit host")
		}

		// Liveness-aware placement: probe eligible hosts and pick the best reachable one
		bestHost, _, err := placement.BestReachableHost(database, placementConstraints, 5*time.Second)
		if err != nil {
			if errors.Is(err, placement.ErrNoReachableHost) {
				// No hosts responded — fall back to predictor-aware static placement
				bestHost, _, err = placement.BestHostWithPredictor(database, placementConstraints, nil, predict)
				if err != nil {
					return fmt.Errorf("auto-placement failed: %w", err)
				}
			} else {
				return fmt.Errorf("auto-placement failed: %w", err)
			}
		}
		host = bestHost
	}

	dirProvided := runDir != ""

	// Parse "cd /path && command" pattern to extract working directory
	// Only if -C/--directory wasn't explicitly provided
	parsedDir, parsedCmd := parseCdPrefix(command)
	if parsedDir != "" && runDir == "" {
		command = parsedCmd
		runDir = parsedDir
		dirProvided = true
	}

	// Set defaults
	workingDir := runDir
	if workingDir == "" {
		// Try automap: if CWD is under a known prefix, use the tilde-relative path
		home, _ := os.UserHomeDir()
		cwd, _ := os.Getwd()
		if home != "" && cwd != "" {
			for _, prefix := range config.AutomapDirs() {
				expanded := strings.Replace(prefix, "~", home, 1)
				if rel, err := filepath.Rel(expanded, cwd); err == nil && !strings.HasPrefix(rel, "..") {
					workingDir = prefix + "/" + rel
					fmt.Fprintf(cmd.ErrOrStderr(), "Auto-detected working directory: %s\n", workingDir)
					break
				}
			}
		}
	}
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

	// Sync sources directly to target host (fallback path, coordinator unreachable)
	if !runNoSync && !runDryRun && !runDraft && host != "" {
		localDir := resolveLocalDir(workingDir)
		if localDir != "" {
			if err := srcsync.SyncSources(host, localDir, workingDir); err != nil {
				_ = err // sync failure is expected in occasionally-connected design
			}
		}

		// Sync extra file paths from --input flags and .weft.yaml
		extraPaths := collectExtraPaths(runInputs, localDir)
		if len(extraPaths) > 0 {
			if err := srcsync.SyncExtraPaths(host, extraPaths); err != nil {
				_ = err // sync failure is expected in occasionally-connected design
			}
		}
	}

	// Log CLI command invocation
	mode := "queue"
	if runImmediate {
		mode = "immediate"
	}
	oplog.Log(oplog.OpCLICommand, oplog.WithHost(host), oplog.WithDetailf("run mode=%s cmd=%s", mode, command))

	if runDraft {
		gpu := extractGPUFromEnvVars(runEnvVars)
		jobID, err := db.RecordDraftJobWithGPU(database, host, workingDir, command, runDescription, gpu)
		if err != nil {
			return fmt.Errorf("record draft job: %w", err)
		}
		backend, err := ops.ResolveBackend(host, 5*time.Second)
		if err != nil {
			return fmt.Errorf("resolve backend: %w", err)
		}
		if err := db.SetJobBackend(database, jobID, backend); err != nil {
			return fmt.Errorf("set job backend: %w", err)
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

	// Resolve GPU, GPU class, and GPU memory reservation
	gpu := extractGPUFromEnvVars(runEnvVars)
	gpuClass := runGPUClass
	// Non-numeric --gpu values (e.g., "A100") are treated as GPU class names
	if gpu != "" && !isNumericGPU(gpu) {
		gpuClass = gpu
		gpu = ""
	}
	gpuMemGB := resolveGPUMemGB(runGPUMem, gpu, gpuClass)

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
			GPU:          gpu,
			GPUClass:     gpuClass,
			GPUMemGB:     gpuMemGB,
			Dependencies: deps,
			AutoStart:    true,
			Inputs:       runInputs,
			Outputs:      runOutputs,
			OutputDirs:   outputDirs,
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
			fmt.Printf("\nJob saved locally. %s is offline — it will be sent to the remote queue on the next sync.\n", host)
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
			GPU:         gpu,
			GPUClass:    gpuClass,
			GPUMemGB:    gpuMemGB,
			AutoStart:   true,
			Inputs:      runInputs,
			Outputs:     runOutputs,
			OutputDirs:  outputDirs,
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

		// Handle --wait: block until job completes
		if runWait {
			return waitForQueuedJobCompletion(database, jobID, res.Deferred)
		}

		// Handle --follow: wait for job to start, then stream logs
		if runFollow {
			return followQueuedJob(database, jobID, host, res.Deferred)
		}

		// Default: show deferred message and return
		if res.Deferred {
			fmt.Printf("\nJob saved locally. %s is offline — it will be sent to the remote queue on the next sync.\n", host)
		} else {
			backend, err := ops.ResolveBackend(host, 5*time.Second)
			if err == nil && backend != db.BackendSlurm {
				// Auto-start queue runner (silently ignore offline errors)
				_, _ = ensureQueueRunnerStarted(host, defaultQueueName)
			}
		}
		return nil
	}

	// --immediate mode: start job now in its own tmux session
	backend, err := ops.ResolveBackend(host, 5*time.Second)
	if err != nil {
		return fmt.Errorf("resolve backend: %w", err)
	}
	if backend == db.BackendSlurm {
		res, err := queueJob(database, queueJobOptions{
			Host:        host,
			WorkingDir:  workingDir,
			Command:     command,
			Description: runDescription,
			EnvVars:     runEnvVars,
			Tags:        runTags,
			GPU:         gpu,
			GPUClass:    gpuClass,
			GPUMemGB:    gpuMemGB,
			AutoStart:   false,
			Inputs:      runInputs,
			Outputs:     runOutputs,
			OutputDirs:  outputDirs,
		})
		if err != nil {
			return fmt.Errorf("queue job: %w", err)
		}
		fmt.Printf("Job #%d submitted to SLURM on %s\n\n", res.JobID, host)
		fmt.Printf("  Working dir: %s\n", workingDir)
		fmt.Printf("  Command: %s\n", command)
		if runDescription != "" {
			fmt.Printf("  Description: %s\n", runDescription)
		}
		if len(runEnvVars) > 0 {
			fmt.Printf("  Env vars: %s\n", strings.Join(runEnvVars, ", "))
		}
		if res.Deferred {
			fmt.Printf("\nJob saved locally. %s is offline — it will be sent to the remote queue on the next sync.\n", host)
		}
		return nil
	}

	result, err := startJob(database, startJobOptions{
		Host:        host,
		WorkingDir:  workingDir,
		Command:     command,
		Description: runDescription,
		EnvVars:     runEnvVars,
		Tags:        runTags,
		GPUMemGB:    gpuMemGB,
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

	fmt.Printf("Job #%d queued on %s\n", result.Info.JobID, result.Info.Host)

	if runWait {
		return waitForQueuedJobCompletion(database, result.Info.JobID, false)
	}

	if runFollow || runAllow {
		return followQueuedJob(database, result.Info.JobID, host, false)
	}

	if usageHintsEnabled() {
		fmt.Printf("\nMonitor progress:\n")
		fmt.Printf("  weft status %d                   # Check status\n", result.Info.JobID)
		fmt.Printf("  weft status --wait %d            # Wait for completion\n", result.Info.JobID)
		fmt.Printf("  weft status --wait --wait-timeout 30m %d  # Wait with timeout\n", result.Info.JobID)
		fmt.Printf("\nView log:\n")
		fmt.Printf("  weft log %d                      # View log\n", result.Info.JobID)
		fmt.Printf("  weft log %d -f                   # Follow log\n", result.Info.JobID)
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

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.ContainsAny(s, " \t\n\"'`$\\~") {
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
	return dirClean == homeClean || strings.HasPrefix(dirClean, homeClean+string(os.PathSeparator))
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
		fmt.Printf("View logs later: weft log %d -f\n", jobID)
		fmt.Printf("Check status:   weft job status %d\n", jobID)
	}
}

// printCommandRecommendations checks for common command patterns and suggests
// better alternatives using CLI flags. Returns true if any recommendations were printed.
func printCommandRecommendations(command string) bool {
	if !usageHintsEnabled() {
		return false
	}
	var recommendations []string

	// Check for "VAR=value " prefix (environment variable)
	trimmed := strings.TrimSpace(command)
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

// coordinatorReachable checks if the coordinator daemon is running.
// It checks for the PID file via SSH with a short timeout for fast fail.
func coordinatorReachable(cfg *config.Config) bool {
	host := cfg.GetCoordinatorHost()
	coordConfig := coordinator.DefaultConfig()
	cmd := fmt.Sprintf("test -f %s && kill -0 $(cat %s) 2>/dev/null && echo YES || echo NO",
		coordConfig.PIDFile, coordConfig.PIDFile)
	stdout, _, err := ssh.RunWithTimeout(host, cmd, 3*time.Second)
	if err != nil {
		return false
	}
	return strings.TrimSpace(stdout) == "YES"
}

// submitIntent writes a placement intent to the coordinator and records the job locally.
// host may be empty (auto-placement) or an explicit host name (passed as a constraint).
func submitIntent(cfg *config.Config, database *sql.DB, host, command, dir, description string, envVars, tags, inputs, outputs []string, gpuClass string, gpuMemGB int, depAfter int64) error {
	// Resolve working directory
	workingDir := dir
	if workingDir == "" {
		// Try automap: if CWD is under a known prefix, use the tilde-relative path
		home, _ := os.UserHomeDir()
		cwd, _ := os.Getwd()
		if home != "" && cwd != "" {
			for _, prefix := range config.AutomapDirs() {
				expanded := strings.Replace(prefix, "~", home, 1)
				if rel, err := filepath.Rel(expanded, cwd); err == nil && !strings.HasPrefix(rel, "..") {
					workingDir = prefix + "/" + rel
					fmt.Fprintf(os.Stderr, "Auto-detected working directory: %s\n", workingDir)
					break
				}
			}
		}
	}
	if workingDir == "" {
		var err error
		workingDir, err = session.DefaultWorkingDir()
		if err != nil {
			return fmt.Errorf("get working dir: %w", err)
		}
	}

	// Sync sources to coordinator (hop 1: CLI → coordinator)
	if !runNoSync {
		syncHost := cfg.GetCoordinatorHost()
		hostname, _ := os.Hostname()
		if hostname != syncHost {
			localDir := resolveLocalDir(workingDir)
			if localDir != "" {
				if err := srcsync.SyncSources(syncHost, localDir, workingDir); err != nil {
					_ = err // sync failure is expected in occasionally-connected design
				}
			}

			// Sync extra file paths from --input flags and .weft.yaml
			extraPaths := collectExtraPaths(inputs, localDir)
			if len(extraPaths) > 0 {
				if err := srcsync.SyncExtraPaths(syncHost, extraPaths); err != nil {
					_ = err // sync failure is expected in occasionally-connected design
				}
			}
		}
	}

	// Record job locally with pending_placement status
	jobID, err := db.RecordQueuedWithGPU(database, "", workingDir, command, description, "")
	if err != nil {
		return fmt.Errorf("record job: %w", err)
	}
	if err := db.MarkPendingPlacement(database, jobID); err != nil {
		return fmt.Errorf("set pending_placement: %w", err)
	}
	if len(tags) > 0 {
		if err := db.SetJobTags(database, jobID, tags); err != nil {
			return fmt.Errorf("set tags: %w", err)
		}
	}
	if len(envVars) > 0 {
		if err := db.SetJobEnvVars(database, jobID, envVars); err != nil {
			return fmt.Errorf("set env vars: %w", err)
		}
	}
	if len(inputs) > 0 {
		if err := db.SetJobInputs(database, jobID, inputs); err != nil {
			return fmt.Errorf("set inputs: %w", err)
		}
	}
	if len(outputs) > 0 {
		if err := db.SetJobOutputs(database, jobID, outputs); err != nil {
			return fmt.Errorf("set outputs: %w", err)
		}
	}

	// Build intent
	var gpuMemPtr *int
	if gpuMemGB > 0 {
		gpuMemPtr = &gpuMemGB
	}

	depSpec := ""
	if depAfter > 0 {
		depSpec = fmt.Sprintf("%d", depAfter)
	}

	hostname, _ := os.Hostname()
	i := &intent.Intent{
		Timestamp: time.Now(),
		Op:        "place",
		IntentID:  uuid.New().String(),
		Source:    hostname,
		Job: intent.IntentJob{
			ID:      jobID,
			Cmd:     command,
			Dir:     workingDir,
			Desc:    description,
			Env:     envVars,
			Inputs:  inputs,
			Outputs: outputs,
			Constraints: intent.IntentConstraints{
				GPUClass: gpuClass,
				GPUMemGB: gpuMemGB,
			},
			Tags:     tags,
			DepSpec:  depSpec,
			GPUMemGB: gpuMemPtr,
		},
	}

	// Store intent ID in remote_id for outcome polling
	if err := db.SetJobRemoteID(database, jobID, i.IntentID); err != nil {
		return fmt.Errorf("store intent ID: %w", err)
	}

	// Write intent to coordinator
	if err := intent.WriteIntent(cfg.GetCoordinatorHost(), i); err != nil {
		// If coordinator write fails, fall back to local placement info
		fmt.Fprintf(os.Stderr, "Warning: could not submit intent to coordinator: %v\n", err)
		fmt.Fprintf(os.Stderr, "Job %d saved locally with pending_placement status\n", jobID)
		return nil
	}

	oplog.Log(oplog.OpCLICommand, oplog.WithDetailf("intent submitted id=%s job=%d", i.IntentID, jobID))

	fmt.Printf("Intent %s submitted to coordinator\n", i.IntentID)
	fmt.Printf("Job #%d saved locally (pending placement)\n\n", jobID)
	fmt.Printf("  Working dir: %s\n", workingDir)
	fmt.Printf("  Command: %s\n", command)
	if description != "" {
		fmt.Printf("  Description: %s\n", description)
	}

	return nil
}

// collectExtraPaths gathers file paths to sync from --input flags and .weft.yaml.
// It classifies --input values into asset refs (ignored here) and file paths,
// then merges with extra_paths from the project config if found.
func collectExtraPaths(inputs []string, localDir string) []string {
	_, filePaths := dataloc.ClassifyInputs(inputs)
	filePaths = append(filePaths, config.ProjectExtraPaths(localDir)...)
	return filePaths
}

// buildJobPredictor creates a placement.JobPredictor from the app config and constraints.
// Returns nil if the predictor is not configured or no command is set.
func buildJobPredictor(cfg *config.Config, c placement.Constraints) placement.JobPredictor {
	pcfg := buildPredictorConfig(cfg)
	if !pcfg.Configured() || c.Command == "" {
		return nil
	}
	return placement.NewJobPredictor(func(host string) *placement.RawPrediction {
		result, err := predictor.Predict(pcfg, host, c.Project, c.GPUClass, c.Command)
		if err != nil {
			return nil
		}
		raw := &placement.RawPrediction{}
		if result.DurationS != nil {
			raw.DurationS = &placement.RawPredictionField{Mean: result.DurationS.Mean, Upper: result.DurationS.Upper}
		}
		if result.PeakRSSKB != nil {
			raw.PeakRSSKB = &placement.RawPredictionField{Mean: result.PeakRSSKB.Mean, Upper: result.PeakRSSKB.Upper}
		}
		if result.MaxGPUMemMiB != nil {
			raw.MaxGPUMemMiB = &placement.RawPredictionField{Mean: result.MaxGPUMemMiB.Mean, Upper: result.MaxGPUMemMiB.Upper}
		}
		return raw
	})
}

// projectFromDir extracts a short project name from a working directory path.
// E.g. "~/code/research/llm-performance-models" → "llm-performance-models".
// Returns "" if the directory is empty.
func projectFromDir(dir string) string {
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return ""
		}
		return filepath.Base(cwd)
	}
	return filepath.Base(dir)
}

// resolveLocalDir converts a tilde-prefixed working directory back to a local
// absolute path using the automap directory prefixes. Returns "" if the path
// cannot be resolved to a local directory.
func resolveLocalDir(workingDir string) string {
	if workingDir == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	for _, prefix := range config.AutomapDirs() {
		expanded := strings.Replace(prefix, "~", home, 1)
		if strings.HasPrefix(workingDir, prefix+"/") {
			rel := workingDir[len(prefix)+1:]
			return filepath.Join(expanded, rel)
		}
		if workingDir == prefix {
			return expanded
		}
	}
	// If workingDir is already an absolute path, use it directly
	if filepath.IsAbs(workingDir) {
		return workingDir
	}
	return ""
}
