package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/runner"
	"github.com/osteele/weft/internal/workdir"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run [flags] <command>",
	Short: "Queue a job on a remote host",
	Long: `Queue a job on a remote host for sequential execution.

If --host is omitted, automatic placement selects the best host based on
GPU constraints (--gpu, --gpu-class, --gpu-mem) and data locality (--input).

Examples:
  weft run 'python train.py'                           # Auto-place on best host
  weft run --gpu nvidia 'python train.py'              # Any NVIDIA GPU
  weft run --gpu nvidia>=24GB 'python train.py'        # Any NVIDIA GPU with 24+ GB
  weft run --gpu ampere+ 'python train.py'             # Ampere or newer
  weft run --gpu a100 'python train.py'                # Place on A100 host
  weft run --gpu-class a100 --gpu-mem 60 'python ...'  # Separate flags still work
  weft run --host cool30 'python train.py'              # Explicit host
  weft run -m "Training" --host cool30 'python train.py'
  weft run --after 42 'python eval.py'                  # Run after job 42
  weft run --wait --host cool30 'python train.py'       # Queue and wait for completion
  weft run -f --host cool30 'python train.py'           # Queue and follow log output`,
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
		// Normal mode: exactly 1 arg (command)
		if len(args) != 1 {
			return fmt.Errorf("requires <command> argument")
		}
		return nil
	}),
	RunE: runRun,
}

var (
	runHost         string
	runDir          string
	runDescription  string
	runProject      string
	runDraft        bool
	runFollow       bool
	runWait         bool
	runNoWait       bool // explicit no-op flag for tooling compatibility
	runKillJobID    int64
	runFrom         int64
	runEnvVars      []string
	runTags         []string
	runAfter        int64
	runAfterAny     int64
	runGPU          string
	runGPUMem       int
	runGPUMemStrict bool
	runGPUClass     string
	runInputs       []string
	runOutputs      []string
	runProduces     []string
	runNeeds        []string
	runDryRun       bool
	runNoSync       bool

	submitJobsToInstanceFunc = campaign.SubmitJobsToInstance
)

const defaultGPUMemGB = ops.DefaultGPUMemGB

const (
	runCloudReuseAckWaitTimeout  = 250 * time.Millisecond
	runCloudReuseAckPollInterval = 50 * time.Millisecond
)

type cloudReuseSubmitOutcome int

const (
	cloudReuseSubmitFailed cloudReuseSubmitOutcome = iota
	cloudReuseAckReceived
	cloudReuseAckNotObserved
)

func submitJobToCloudReuse(database *sql.DB, r2Client *r2.Client, instanceID int64, job *db.Job) (cloudReuseSubmitOutcome, time.Duration, error) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), runCloudReuseAckWaitTimeout)
	defer cancel()
	ctx = controlplane.WithGraceAckPollInterval(ctx, runCloudReuseAckPollInterval)

	err := submitJobsToInstanceFunc(ctx, database, r2Client, instanceID, []*db.Job{job})
	duration := time.Since(start)
	if err == nil {
		return cloudReuseAckReceived, duration, nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return cloudReuseAckNotObserved, duration, nil
	}
	return cloudReuseSubmitFailed, duration, err
}

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().StringVarP(&runHost, "host", "H", "", "Remote host to run on (default: auto-place)")
	runCmd.Flags().BoolVar(&runDraft, "draft", false, "Create the job in draft status without contacting remote hosts")
	runCmd.Flags().StringVarP(&runDir, "directory", "C", "", "Working directory (default: current directory path; alias: --dir)")
	runCmd.Flags().StringVar(&runProject, "project", "", "Project name (default: repo root name for the working directory)")
	runCmd.Flags().StringVarP(&runDescription, "message", "m", "", "Description of the job")
	runCmd.Flags().StringVarP(&runDescription, "description", "d", "", "[deprecated: use -m] Description of the job")
	runCmd.Flags().MarkHidden("description")
	runCmd.Flags().BoolVarP(&runFollow, "follow", "f", false, "Follow log output after starting")
	runCmd.Flags().Int64Var(&runKillJobID, "kill", 0, "Kill a job by ID (synonym for 'weft kill')")
	runCmd.Flags().Int64Var(&runFrom, "from", 0, "Copy settings from existing job ID before running")
	runCmd.Flags().StringSliceVarP(&runEnvVars, "env", "e", nil, "Environment variable (VAR=value), can be repeated")
	runCmd.Flags().StringSliceVar(&runTags, "tag", nil, "Tag to attach to the job (can be repeated). Reserved tags: 'exclusive' runs alone; 'benchmark' waits for system-wide idle; 'rental' skips local placement; 'inventory' blocks rental placement; 'preemptible' allows interruptible cloud placement")
	runCmd.Flags().Int64Var(&runAfter, "after", 0, "Start job after another job succeeds (implies --queue)")
	runCmd.Flags().Int64Var(&runAfter, "depends-on", 0, "Alias for --after; start job after another job succeeds (implies --queue)")
	runCmd.Flags().Int64Var(&runAfterAny, "after-any", 0, "Start job after another job completes, success or failure (implies --queue)")
	runCmd.Flags().StringVar(&runGPU, "gpu", "", "GPU constraint: class, generation, or family (e.g., a100, ampere+, nvidia); append >=NGB for memory (e.g., nvidia>=24GB)")
	runCmd.Flags().IntVar(&runGPUMem, "gpu-mem", 0, "GPU memory reservation in GB per device (default: 20 when GPU is used)")
	runCmd.Flags().BoolVar(&runGPUMemStrict, "gpu-mem-strict", false, "Use exact gpu-mem matching without default safety headroom")
	runCmd.Flags().StringVar(&runGPUClass, "gpu-class", "", "GPU class or generation (e.g., a100, ampere, ampere+); '+' means that generation or newer")
	runCmd.Flags().BoolVar(&runWait, "wait", false, "Wait for job to complete before returning")
	runCmd.Flags().BoolVar(&runNoWait, "no-wait", false, "Don't wait for job (default behavior, for explicit acknowledgment)")
	runCmd.Flags().StringSliceVar(&runInputs, "input", nil, "Input data asset (e.g., hf:meta-llama/Llama-3-8B), can be repeated")
	runCmd.Flags().StringSliceVar(&runOutputs, "output", nil, "Output data asset (e.g., checkpoint:llama-ft-v1), can be repeated")
	runCmd.Flags().StringSliceVar(&runProduces, "produces", nil, "Artifact path this job produces (repeatable, e.g., output/model.pt or output/model.pt:100)")
	runCmd.Flags().StringSliceVar(&runNeeds, "needs", nil, "Artifact path:version this job needs (repeatable, e.g., output/model.pt:100)")
	runCmd.Flags().BoolVar(&runDryRun, "dry-run", false, "Show placement scores without submitting the job")
	runCmd.Flags().BoolVar(&runNoSync, "no-sync", false, "Skip source sync before submission")
	addJobAddFlagAliases(runCmd)
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
	localNeeds := append([]string(nil), runNeeds...)
	cloudNeeds := []string{}

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

		// Copy settings from existing job (explicit flags take priority)
		if host == "" && fromJob.HasInventoryHost() {
			host = fromJob.Host
		}
		command = fromJob.Command
		if runDir == "" {
			runDir = fromJob.WorkingDir
		}
		if runDescription == "" {
			runDescription = fromJob.Description
		}
		if runProject == "" {
			runProject = fromJob.Project
		}
		if len(runTags) == 0 {
			runTags = append([]string(nil), fromJob.Tags...)
		}
		if runGPUClass == "" && runGPU == "" {
			runGPUClass = fromJob.GPUClass
		}
		if runGPUMem == 0 && fromJob.GPUMemGB != nil {
			runGPUMem = *fromJob.GPUMemGB
		}
		if len(runEnvVars) == 0 {
			runEnvVars = append([]string(nil), fromJob.EnvVars...)
		}
		if len(runInputs) == 0 {
			runInputs = append([]string(nil), fromJob.Inputs...)
		}
		if len(runOutputs) == 0 {
			runOutputs = append([]string(nil), fromJob.Outputs...)
		}
		if len(runProduces) == 0 {
			runProduces = append([]string(nil), fromJob.Produces...)
		}
		if len(runNeeds) == 0 {
			runNeeds = append([]string(nil), fromJob.Needs...)
		}

		// Allow overriding command from positional arg
		if len(args) > 0 {
			command = args[0]
		}
	} else {
		// Parse positional args
		if len(args) == 1 {
			command = args[0]
		}
	}

	// Validate command against blocked patterns
	cfg, _ := config.Load()
	setUsageHintsFromConfig(cfg)
	if err := cfg.ValidateCommand(command); err != nil {
		return err
	}
	// Skip predictor entirely when placement won't use it: explicit --host,
	// rental-tagged jobs (skip on-prem placement), or draft submissions. This
	// avoids both the readiness check and the later gpu-mem prediction path
	// (which both cold-start `uv run` and dominate submission latency).
	predictorNeeded := host == "" && !db.HasRentalTag(runTags) && !runDraft
	if predictorNeeded {
		if err := ensurePredictorUsableFunc(cmd, cfg, "placement prediction"); err != nil {
			return err
		}
	}

	// Parse "cd /path && command" pattern to extract working directory
	// Only if -C/--directory wasn't explicitly provided
	dirExplicit := runDir != ""
	parsedDir, parsedCmd := parseCdPrefix(command)
	if parsedDir != "" && runDir == "" {
		command = parsedCmd
		runDir = parsedDir
		dirExplicit = true
	}

	// Resolve working directory
	workingDir, err := workdir.ResolveWorkingDir(runDir, cmd.ErrOrStderr())
	if err != nil {
		return fmt.Errorf("get working dir: %w", err)
	}

	// Warn about literal local home paths (e.g. /Users/osteele/...) that won't exist on remote
	if dirExplicit {
		maybeWarnHomePrefixedDir(host, workingDir)
	}

	projectName, err := workdir.ResolveProjectName(runProject, workingDir)
	if err != nil {
		return fmt.Errorf("resolve project: %w", err)
	}

	// Load output directories from .weft.toml for convention-based output collection
	localDir := workdir.ResolveLocal(workingDir)
	outputDirs := config.ProjectOutputDirs(localDir)

	// Merge project-level inputs with CLI --input flags
	originalRunInputs := append([]string(nil), runInputs...)
	projectInputs := config.ProjectInputs(localDir)
	runInputs = mergeDedup(projectInputs, runInputs)
	inputsBeforeAutoDetect := mergeDedup(projectInputs, originalRunInputs)

	// Auto-detect HF inputs from Python source and command string.
	if detected := dataloc.ScanPythonHFRefsForCommand(localDir, command); len(detected) > 0 {
		runInputs = mergeDedup(runInputs, detected)
	}
	if detected := dataloc.ScanCommandHFRefs(command); len(detected) > 0 {
		runInputs = mergeDedup(runInputs, detected)
	}
	if newInputs := filterNew(runInputs, inputsBeforeAutoDetect); len(newInputs) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Auto-detected inputs: %s\n", strings.Join(newInputs, ", "))
	}

	// Capture CLI intent for resource flags BEFORE script defaults are merged in.
	// These overrides are persisted on the job so retries can replay the user's
	// original submission intent against updated script metadata.
	cliOverrides := &db.CLIResourceOverrides{}
	if runGPU != "" {
		cliOverrides.GPU = runGPU
	}
	if runGPUClass != "" {
		cliOverrides.GPUClass = runGPUClass
	}
	if runGPUMem != 0 {
		mem := runGPUMem
		cliOverrides.GPUMemGB = &mem
	}
	if cmd.Flags().Changed("gpu-mem-strict") {
		s := runGPUMemStrict
		cliOverrides.GPUMemStrict = &s
	}

	// Apply PEP 723 [tool.weft] script metadata as defaults (CLI flags take precedence).
	scriptMeta, scriptMetaErr := dataloc.ScanScriptMeta(localDir, command)
	if scriptMetaErr != nil {
		slog.Warn("script metadata error", "error", scriptMetaErr)
	}
	if meta := scriptMeta; meta != nil {
		var applied []string
		if runGPU == "" && runGPUClass == "" && meta.GPU != "" {
			runGPU = meta.GPU
			applied = append(applied, fmt.Sprintf("gpu=%s", meta.GPU))
		}
		if runGPU == "" && runGPUClass == "" && meta.GPUClass != "" {
			runGPUClass = meta.GPUClass
			applied = append(applied, fmt.Sprintf("gpu-class=%s", meta.GPUClass))
		}
		if runGPUMem == 0 && meta.GPUMemGB > 0 {
			runGPUMem = meta.GPUMemGB
			applied = append(applied, fmt.Sprintf("gpu-mem=%dGB", meta.GPUMemGB))
		}
		if !cmd.Flags().Changed("gpu-mem-strict") && meta.GPUMemStrict != nil {
			runGPUMemStrict = *meta.GPUMemStrict
			applied = append(applied, fmt.Sprintf("gpu-mem-strict=%t", runGPUMemStrict))
		}
		if len(meta.Inputs) > 0 {
			runInputs = mergeDedup(runInputs, meta.Inputs)
			applied = append(applied, fmt.Sprintf("inputs=%v", meta.Inputs))
		}
		if len(meta.Outputs) > 0 {
			runOutputs = mergeDedup(runOutputs, meta.Outputs)
			applied = append(applied, fmt.Sprintf("outputs=%v", meta.Outputs))
		}
		if len(meta.Tags) > 0 {
			runTags = mergeDedup(runTags, meta.Tags)
			applied = append(applied, fmt.Sprintf("tags=%v", meta.Tags))
		}
		if meta.Preemptible {
			runTags = mergeDedup(runTags, []string{db.TagPreemptible})
			applied = append(applied, "preemptible=true")
		}
		if meta.Image != "" {
			applied = append(applied, fmt.Sprintf("image=%s", meta.Image))
		}
		if len(meta.UvArgs) > 0 {
			command = dataloc.ApplyUvArgs(command, meta.UvArgs)
			applied = append(applied, fmt.Sprintf("uv-args=%v", meta.UvArgs))
		}
		if meta.PreInstall != "" {
			command = meta.PreInstall + " && " + command
			applied = append(applied, fmt.Sprintf("pre-install=%s", meta.PreInstall))
		}
		if len(meta.Env) > 0 {
			runEnvVars = append(runEnvVars, applyEnvMap(meta.Env)...)
			applied = append(applied, fmt.Sprintf("env=%v", meta.Env))
		}
		if len(applied) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "Script metadata: %s\n", strings.Join(applied, ", "))
		}
	}

	// Print recommendations for common patterns
	printCommandRecommendations(command)

	// Validate flag combinations
	if runFollow && runAfter > 0 {
		return fmt.Errorf("--follow cannot be used with --after/--depends-on")
	}
	if runFollow && runAfterAny > 0 {
		return fmt.Errorf("--follow cannot be used with --after-any")
	}
	if runAfter > 0 && runAfterAny > 0 {
		return fmt.Errorf("cannot use both --after/--depends-on and --after-any")
	}
	if runDraft && runFollow {
		return fmt.Errorf("--draft cannot be combined with --follow")
	}
	if runDraft && (runAfter > 0 || runAfterAny > 0) {
		return fmt.Errorf("--draft cannot be combined with --after/--depends-on or --after-any")
	}
	if runWait && runFollow {
		return fmt.Errorf("--wait cannot be used with --follow")
	}
	if runWait && runDraft {
		return fmt.Errorf("--wait cannot be combined with --draft")
	}
	if runWait && runNoWait {
		return fmt.Errorf("--wait and --no-wait cannot be used together")
	}

	// Resolve --gpu into --gpu-class (and optionally --gpu-mem)
	if runGPU != "" {
		if runGPUClass != "" {
			return fmt.Errorf("--gpu and --gpu-class cannot be used together")
		}
		parsedClass, parsedMem, err := parseGPUFlag(runGPU)
		if err != nil {
			return fmt.Errorf("--gpu: %w", err)
		}
		runGPUClass = parsedClass
		if parsedMem > 0 {
			if runGPUMem > 0 {
				return fmt.Errorf("--gpu with >=NGB and --gpu-mem cannot be used together")
			}
			runGPUMem = parsedMem
		}
	}

	// Validate --needs entries have valid path:version format
	for _, spec := range runNeeds {
		if _, err := runner.ParseNeedsSpec(spec); err != nil {
			return fmt.Errorf("--needs: %w", err)
		}
	}
	if len(runNeeds) > 0 {
		resolvedHost, resolvedLocalNeeds, resolvedCloudNeeds, err := resolveArtifactNeedsHost(database, runNeeds, host)
		if err != nil {
			return fmt.Errorf("--needs: %w", err)
		}
		host = resolvedHost
		localNeeds = resolvedLocalNeeds
		cloudNeeds = resolvedCloudNeeds
	}

	gpu := extractGPUFromEnvVars(runEnvVars)
	gpuClass := runGPUClass
	// Non-numeric GPU env values are treated as class names.
	if gpu != "" && !isNumericGPU(gpu) {
		gpuClass = gpu
		gpu = ""
	}
	// Query OOM history for this command
	oomFloor, _ := db.OOMFloor(database, command)
	if oomFloor > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "OOM history: requiring >=%dGB GPU memory (prior failure on %dGB GPU)\n", oomFloor, oomFloor-1)
	}
	gpuMemCfg := cfg
	if !predictorNeeded {
		gpuMemCfg = nil // Skip predictor shell-out; use explicit value or fallback.
	}
	resolvedGPUMemGB, resolvedGPUMemMaxGB, _ := resolveEffectiveGPUMemAndCeiling(gpuMemCfg, intPtrOrNil(runGPUMem), gpu, gpuClass, runGPUMemStrict, host, projectName, command, oomFloor)

	// Placement scoring (used for auto-placement and dry-run)
	placementConstraints := placement.Constraints{
		GPUClass: gpuClass,
		Inputs:   runInputs,
		Command:  command,
		Project:  projectName,
		Tags:     runTags,
	}
	if resolvedGPUMemGB != nil {
		placementConstraints.GPUMemGB = *resolvedGPUMemGB
	}

	// Build predictor closure if configured
	predict := placement.BuildJobPredictorFromConfig(cfg, placementConstraints)

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

	relayCfg, relayClient, err := loadCoordinatorRelay()
	if err != nil {
		return err
	}
	if relayEnabled(relayCfg, relayClient) && !runDraft {
		if runWait || runFollow {
			return fmt.Errorf("--wait and --follow are not supported when coordinator relay mode is active")
		}
		if runAfter > 0 || runAfterAny > 0 {
			depID := runAfter
			if depID == 0 {
				depID = runAfterAny
			}
			depJob, err := db.GetJobByID(database, depID)
			if err != nil {
				return fmt.Errorf("get dependency job %d: %w", depID, err)
			}
			if depJob == nil {
				return fmt.Errorf("dependency job %d not found", depID)
			}
			if host == "" {
				host = depJob.Host
			}
			if depJob.Host != host {
				return fmt.Errorf("dependency job %d runs on host %s; relay submission must target the same host %s", depID, depJob.Host, host)
			}
		}
		if err := validatePinnedHostQueueGate(host, gpuClass); err != nil {
			return err
		}

		params := ops.QueueJobParams{
			Host:        host,
			WorkingDir:  workingDir,
			Command:     command,
			Description: runDescription,
			Project:     projectName,
			EnvVars:     runEnvVars,
			Tags:        runTags,
			GPUClass:    gpuClass,
			GPUMemGB:    resolvedGPUMemGB,
			GPUMemMaxGB: resolvedGPUMemMaxGB,
			DepSpec:     encodeQueueDependencies(buildRunDependencies()),
			Inputs:      runInputs,
			Outputs:     runOutputs,
			OutputDirs:  outputDirs,
			Produces:    runProduces,
			Needs:       localNeeds,
			Metadata:    buildCloudDependencyMetadata(nil, cloudNeeds),
		}
		jobID, ack, err := relaySubmitJob(database, relayCfg, relayClient, params)
		if err != nil {
			return err
		}
		if err := db.SetJobCLIResourceOverrides(database, jobID, cliOverrides); err != nil {
			slog.Warn("failed to save cli overrides", "error", err)
		}
		w := cmd.OutOrStdout()
		if ack != nil && ack.Host != "" {
			fmt.Fprintf(w, "Job #%d submitted to coordinator and queued on %s\n", jobID, ack.Host)
		} else {
			fmt.Fprintf(w, "Job #%d submitted to coordinator\n", jobID)
		}
		if ack != nil && ack.Message != "" {
			fmt.Fprintf(w, "  %s\n", ack.Message)
		}
		fmt.Fprintf(w, "  Working dir: %s\n", workingDir)
		fmt.Fprintf(w, "  Command: %s\n", command)
		if runDescription != "" {
			fmt.Fprintf(w, "  Description: %s\n", runDescription)
		}
		return nil
	}

	// Route through local placement for non-draft, non-dependency submissions
	if !runDraft && runAfter == 0 && runAfterAny == 0 {
		var placementResult *placement.PlacementResult
		var placementPlan *placement.PlacementPlan

		if host == "" {
			// Build sources: on-prem + cloud reuse
			sources := []placement.CandidateSource{&placement.OnPremSource{}, &campaign.ReuseSource{}}
			plan, err := placement.Evaluate(placement.EvaluateRequest{
				Constraints: placementConstraints,
				Predictor:   predict,
				Sources:     sources,
				Database:    database,
			})
			if err != nil {
				return err
			}

			// Pick the "fast" strategy (survival-adjusted wallclock)
			pick := plan.Fast
			if pick == nil {
				pick = plan.Cheap
			}

			if pick != nil && pick.Kind == placement.CandidateOnPrem && pick.OnPrem != nil {
				placementResult = pick.OnPrem
				host = pick.OnPrem.Host
				oplog.Log(oplog.OpPlacementDecided,
					oplog.WithHost(host),
					oplog.WithDetail(placement.FormatPlacementDetail(pick.OnPrem)))
			} else if pick != nil && pick.Kind == placement.CandidateCloudReuse && pick.Reuse != nil {
				// Cloud reuse selected — will handle after job creation
				placementPlan = plan
			}
			// else: unplaced
		}

		// Build queue params
		params := ops.QueueJobParams{
			Host:        host,
			WorkingDir:  workingDir,
			Command:     command,
			Description: runDescription,
			Project:     projectName,
			EnvVars:     runEnvVars,
			Tags:        runTags,
			GPUClass:    gpuClass,
			GPUMemGB:    resolvedGPUMemGB,
			GPUMemMaxGB: resolvedGPUMemMaxGB,
			Inputs:      runInputs,
			Outputs:     runOutputs,
			OutputDirs:  outputDirs,
			Produces:    runProduces,
			Needs:       localNeeds,
			Metadata:    buildCloudDependencyMetadata(nil, cloudNeeds),
		}

		if err := validatePinnedHostQueueGate(host, gpuClass); err != nil {
			return err
		}

		jobID, err := ops.RecordQueuedJob(database, params)
		if err != nil {
			return fmt.Errorf("submit job: %w", err)
		}
		if err := db.SetJobCLIResourceOverrides(database, jobID, cliOverrides); err != nil {
			slog.Warn("failed to save cli overrides", "error", err)
		}

		// Store placement telemetry if auto-placement was used
		if placementResult != nil {
			meta := buildPlacementMeta(placementResult, predict)
			if err := db.SetJobPlacementMeta(database, jobID, meta); err != nil {
				slog.Warn("failed to save placement meta", "error", err)
			}
		}

		if host == "" {
			// If Evaluate selected cloud reuse, submit the job now
			if placementPlan != nil && placementPlan.Fast != nil && placementPlan.Fast.Reuse != nil {
				r2Client, err := newR2ClientFromConfig()
				if err == nil {
					job, _ := db.GetJobByID(database, jobID)
					if job != nil {
						instanceID := placementPlan.Fast.Reuse.InstanceID
						outcome, dur, submitErr := submitJobToCloudReuse(database, r2Client, instanceID, job)
						switch outcome {
						case cloudReuseAckReceived:
							oplog.Log(oplog.OpCloudSetJobInstance,
								oplog.WithJobID(jobID),
								oplog.WithDetailf("instance=%d ack=received", instanceID),
								oplog.WithDuration(dur))
							fmt.Printf("Job #%d submitted to rental instance %s (%s)\n",
								jobID, ids.FormatInstanceID(instanceID), placementPlan.Fast.Reuse.DisplayName)
							return nil
						case cloudReuseAckNotObserved:
							oplog.Log(oplog.OpCloudSetJobInstance,
								oplog.WithJobID(jobID),
								oplog.WithDetailf("instance=%d ack=not_observed timeout_ms=%d", instanceID, runCloudReuseAckWaitTimeout.Milliseconds()),
								oplog.WithDuration(dur))
							fmt.Printf("Job #%d sent to rental instance %s (%s); acknowledgment not yet received. Check status later.\n",
								jobID, ids.FormatInstanceID(instanceID), placementPlan.Fast.Reuse.DisplayName)
							return nil
						default:
							oplog.Log(oplog.OpCloudSetJobInstance,
								oplog.WithJobID(jobID),
								oplog.WithDetailf("instance=%d ack=error", instanceID),
								oplog.WithDuration(dur),
								oplog.WithError(submitErr))
						}
					}
				}
			}
			if reasons, reasonErr := placement.ExplainUnplaced(database, placementConstraints); reasonErr != nil {
				slog.Warn("failed to explain unplaced job", "job_id", jobID, "error", reasonErr)
			} else if err := db.SetJobPlacementReasons(database, jobID, reasons); err != nil {
				slog.Warn("failed to save unplaced reasons", "job_id", jobID, "error", err)
			}
			if cfg.ShowRentalHints {
				printUnplacedJobMessage(cmd.OutOrStdout(), jobID, placementConstraints, placementResult)
			}
			return nil
		}

		w := cmd.OutOrStdout()
		fmt.Fprintf(w, "Job #%d queued on %s\n", jobID, host)
		if placementResult != nil && placementResult.CompletionEst.Mean > 0 {
			est := placementResult.CompletionEst
			fmt.Fprintf(w, "  Est. completion: ~%.0fm", est.Mean.Minutes())
			// Show breakdown if any component is significant
			parts := []string{}
			if placementResult.Scores != nil {
				for _, s := range placementResult.Scores {
					if s.Host == host {
						if s.QueueDrainEst.Mean > 0 {
							parts = append(parts, fmt.Sprintf("%.0fm queue", s.QueueDrainEst.Mean.Minutes()))
						}
						if s.TransferEst.Mean > 0 {
							parts = append(parts, fmt.Sprintf("%.0fm transfer", s.TransferEst.Mean.Minutes()))
						}
						parts = append(parts, fmt.Sprintf("%.0fm run", s.RunEst.Mean.Minutes()))
						break
					}
				}
			}
			if len(parts) > 0 {
				fmt.Fprintf(w, " (%s)", strings.Join(parts, " + "))
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "  Working dir: %s\n", workingDir)
		fmt.Fprintf(w, "  Command: %s\n", command)
		if runDescription != "" {
			fmt.Fprintf(w, "  Description: %s\n", runDescription)
		}
		if len(runEnvVars) > 0 {
			fmt.Fprintf(w, "  Env vars: %s\n", strings.Join(runEnvVars, ", "))
		}

		// Push the job to the remote host before waiting/following.
		deferred := !syncHostQuietly(database, host, runNoSync)

		if runWait {
			return waitForQueuedJobCompletion(database, jobID, deferred)
		}
		if runFollow {
			return followQueuedJob(database, jobID, host, deferred)
		}
		if deferred {
			fmt.Fprintf(w, "\nJob saved locally. %s is offline — it will be sent to the remote queue on the next sync.\n", host)
		}
		return nil
	}

	// Below: --draft or dependency modes only

	// Placement for non-scheduler paths (--draft, --after)
	var placementResult *placement.PlacementResult
	if host == "" {
		plan, err := placement.Evaluate(placement.EvaluateRequest{
			Constraints: placementConstraints,
			Predictor:   predict,
			Sources:     []placement.CandidateSource{&placement.OnPremSource{}},
			Database:    database,
		})
		if err != nil {
			return fmt.Errorf("auto-placement failed: %w", err)
		}
		if !plan.Unplaced && plan.Cheap != nil && plan.Cheap.OnPrem != nil {
			placementResult = plan.Cheap.OnPrem
			host = plan.Cheap.OnPrem.Host
			oplog.Log(oplog.OpPlacementDecided,
				oplog.WithHost(host),
				oplog.WithDetail(placement.FormatPlacementDetail(plan.Cheap.OnPrem)))
		}
	}

	// Unplaced jobs: record locally and prompt for cloud launch
	if host == "" {
		jobID, err := ops.RecordQueuedJob(database, ops.QueueJobParams{
			WorkingDir:  workingDir,
			Command:     command,
			Description: runDescription,
			Project:     projectName,
			EnvVars:     runEnvVars,
			Tags:        runTags,
			GPUClass:    gpuClass,
			GPUMemGB:    resolvedGPUMemGB,
			GPUMemMaxGB: resolvedGPUMemMaxGB,
			Inputs:      runInputs,
			Outputs:     runOutputs,
			OutputDirs:  outputDirs,
			Produces:    runProduces,
			Needs:       localNeeds,
			Metadata:    buildCloudDependencyMetadata(nil, cloudNeeds),
		})
		if err != nil {
			return fmt.Errorf("record unplaced job: %w", err)
		}
		if err := db.SetJobCLIResourceOverrides(database, jobID, cliOverrides); err != nil {
			slog.Warn("failed to save cli overrides", "error", err)
		}
		if reasons, reasonErr := placement.ExplainUnplaced(database, placementConstraints); reasonErr != nil {
			slog.Warn("failed to explain unplaced job", "job_id", jobID, "error", reasonErr)
		} else if err := db.SetJobPlacementReasons(database, jobID, reasons); err != nil {
			slog.Warn("failed to save unplaced reasons", "job_id", jobID, "error", err)
		}
		if cfg.ShowRentalHints {
			printUnplacedJobMessage(cmd.OutOrStdout(), jobID, placementConstraints, placementResult)
		}
		return nil
	}

	oplog.Log(oplog.OpCLICommand, oplog.WithHost(host), oplog.WithDetailf("run mode=queue cmd=%s", command))

	if err := validatePinnedHostQueueGate(host, gpuClass); err != nil {
		return err
	}

	if runDraft {
		jobID, err := db.RecordDraftJobWithGPU(database, host, workingDir, command, runDescription, gpu)
		if err != nil {
			return fmt.Errorf("record draft job: %w", err)
		}
		if err := db.SetJobCLIResourceOverrides(database, jobID, cliOverrides); err != nil {
			slog.Warn("failed to save cli overrides", "error", err)
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
		if gpuClass != "" {
			if err := db.SetJobGPUClass(database, jobID, gpuClass); err != nil {
				return fmt.Errorf("set GPU class: %w", err)
			}
		}
		if resolvedGPUMemGB != nil {
			if err := db.SetJobGPUMemGB(database, jobID, resolvedGPUMemGB); err != nil {
				return fmt.Errorf("set GPU memory: %w", err)
			}
		}
		if resolvedGPUMemMaxGB != nil {
			if err := db.SetJobGPUMemMaxGB(database, jobID, resolvedGPUMemMaxGB); err != nil {
				return fmt.Errorf("set GPU memory ceiling: %w", err)
			}
		}
		if projectName != "" {
			if err := db.SetJobProject(database, jobID, projectName); err != nil {
				return fmt.Errorf("set project: %w", err)
			}
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
		cloudAfter := []db.JobDependencyRef{}
		waitType := "succeeds"
		afterID := runAfter
		if runAfterAny > 0 {
			afterID = runAfterAny
			waitType = "completes"
			localDep, cloudDep, err := resolveDependencyForTarget(database, afterID, host, true)
			if err != nil {
				return err
			}
			if localDep != nil {
				deps = append(deps, *localDep)
			}
			if cloudDep != nil {
				cloudAfter = append(cloudAfter, *cloudDep)
			}
		} else if runAfter > 0 {
			localDep, cloudDep, err := resolveDependencyForTarget(database, afterID, host, false)
			if err != nil {
				return err
			}
			if localDep != nil {
				deps = append(deps, *localDep)
			}
			if cloudDep != nil {
				cloudAfter = append(cloudAfter, *cloudDep)
			}
		}
		res, err := queueJob(database, queueJobOptions{
			Host:         host,
			WorkingDir:   workingDir,
			Command:      command,
			Description:  runDescription,
			Project:      projectName,
			EnvVars:      runEnvVars,
			Tags:         runTags,
			GPU:          gpu,
			GPUClass:     gpuClass,
			GPUMemGB:     resolvedGPUMemGB,
			GPUMemMaxGB:  resolvedGPUMemMaxGB,
			Dependencies: deps,
			AutoStart:    true,
			Inputs:       runInputs,
			Outputs:      runOutputs,
			OutputDirs:   outputDirs,
			Produces:     runProduces,
			Needs:        localNeeds,
			CloudAfter:   cloudAfter,
			CloudNeeds:   cloudNeeds,
			GPUMemStrict: true, // GPUMemGB is already resolved above; avoid re-applying headroom.
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

		syncAndReportOffline(database, host, runNoSync)
		return nil
	}

	return nil
}

// printUnplacedJobMessage prints user-facing output when a job has no eligible local host.
func printUnplacedJobMessage(w io.Writer, jobID int64, constraints placement.Constraints, result *placement.PlacementResult) {
	if result != nil && result.SpilledToRental {
		// Auto-spill: on-prem was eligible but rental is faster
		fmt.Fprintf(w, "Job #%d needs rental (on-prem est. ~%.0fm",
			jobID, result.CompletionEst.Mean.Minutes())
		if result.RentalEst != nil {
			fmt.Fprintf(w, " vs rental ~%.0fm", result.RentalEst.Mean.Minutes())
		}
		fmt.Fprintln(w, ")")
	} else {
		constraintDesc := placement.DescribeConstraints(constraints)
		fmt.Fprintf(w, "No local host matches constraints: %s\n", constraintDesc)
		if db.HasInventoryTag(constraints.Tags) {
			fmt.Fprintf(w, "Job #%d accepted (waiting for inventory capacity)\n", jobID)
			fmt.Fprintf(w, "This job is inventory-only and will not launch on rental GPUs.\n")
			return
		}
		fmt.Fprintf(w, "Job #%d accepted (needs rental host)\n", jobID)
	}
	fmt.Fprintf(w, "Use 'weft instance launch' to launch on a rental GPU.\n")
}

// buildPlacementMeta extracts telemetry from a placement result and optional predictor.
func buildPlacementMeta(result *placement.PlacementResult, predict placement.JobPredictor) *db.PlacementMeta {
	meta := &db.PlacementMeta{}

	// Extract prediction for the selected host
	if predict != nil {
		p := predict(result.Host)
		if p != nil {
			meta.PredictedDurationS = p.DurationS
			meta.PredictedRSSKB = p.PeakRSSKB
			meta.PredictedGPUMemMiB = p.MaxGPUMemMiB
		}
	}

	// Extract selected and runner-up scores in a single pass
	foundRunnerUp := false
	for _, s := range result.Scores {
		if s.Host == result.Host {
			meta.SelectedScore = s.Total
		} else if !foundRunnerUp && s.Eligible {
			meta.RunnerUpHost = s.Host
			meta.RunnerUpScore = s.Total
			foundRunnerUp = true
		}
	}

	return meta
}

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
	rel, _ := filepath.Rel(home, dir)
	fmt.Fprintf(os.Stderr, "\nWarning: local path %s may not exist on %s.\n", dir, host)
	fmt.Fprintf(os.Stderr, "Use -C '~/%s' for the remote home-relative path.\n", rel)
}

func pathHasHomePrefix(dir, home string) bool {
	dirClean := filepath.Clean(dir)
	homeClean := filepath.Clean(home)
	return dirClean == homeClean || strings.HasPrefix(dirClean, homeClean+string(os.PathSeparator))
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

// mergeDedup merges two string slices, removing duplicates. Items from a appear
// first, then unique items from b.
func mergeDedup(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	seen := make(map[string]bool, len(a)+len(b))
	var result []string
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}
	return result
}

// filterNew returns items that exist in items but not in baseline.
func filterNew(items, baseline []string) []string {
	if len(items) == 0 {
		return nil
	}
	if len(baseline) == 0 {
		return append([]string(nil), items...)
	}
	seen := make(map[string]bool, len(baseline))
	for _, s := range baseline {
		seen[s] = true
	}
	var out []string
	for _, s := range items {
		if !seen[s] {
			out = append(out, s)
		}
	}
	return out
}

// intPtrOrNil returns a pointer to v if v > 0, or nil otherwise.
func intPtrOrNil(v int) *int {
	if v > 0 {
		return &v
	}
	return nil
}

func buildRunDependencies() []queueDependency {
	var deps []queueDependency
	if runAfter > 0 {
		deps = append(deps, queueDependency{JobID: runAfter})
	}
	if runAfterAny > 0 {
		deps = append(deps, queueDependency{JobID: runAfterAny, AllowFailure: true})
	}
	return deps
}

// syncHostQuietly syncs a host to push queued jobs to the remote.
// Returns true if the host was contacted, false if offline or skipped.
func syncHostQuietly(database *sql.DB, host string, noSync bool) bool {
	if host == "" || noSync {
		return false
	}
	syncResult, _ := ops.SyncHost(database, host, ops.HostSyncOptions{
		Timeout: 10 * time.Second,
		Logger:  ops.NewSilentSyncLogger(),
	}, func(h string) (bool, error) {
		return ensureQueueRunnerStarted(h, defaultQueueName)
	})
	return syncResult.HostContacted
}

// syncAndReportOffline syncs a host and prints an offline message if unreachable.
func syncAndReportOffline(database *sql.DB, host string, noSync bool) {
	if noSync {
		fmt.Printf("\nSync skipped (--no-sync). Job will be sent to the remote queue on the next background sync.\n")
		return
	}
	if !syncHostQuietly(database, host, false) && host != "" {
		fmt.Printf("\nJob saved locally. %s is offline — it will be sent to the remote queue on the next sync.\n", host)
	}
}
