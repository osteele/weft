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
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/osteele/weft/internal/bidding"
	"github.com/osteele/weft/internal/blockreason"
	"github.com/osteele/weft/internal/campaign"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/dataplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/localmutate"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/placement"
	"github.com/osteele/weft/internal/r2"
	"github.com/osteele/weft/internal/runner"
	srcsync "github.com/osteele/weft/internal/sync"
	"github.com/osteele/weft/internal/workdir"
	toml "github.com/pelletier/go-toml"
	"github.com/spf13/cobra"
)

var runCmd = &cobra.Command{
	Use:   "run [flags] <command>",
	Short: "Queue a job on a remote host",
	Long: `Queue a job for managed execution on a remote host.

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
  weft run --after wj42 'python eval.py'                # Run after job wj42
  weft run --wait --host cool30 'python train.py'       # Queue and wait for completion
  weft run -f --host cool30 'python train.py'           # Queue and follow log output`,
	Args: usageArgs(func(cmd *cobra.Command, args []string) error {
		// --kill mode: no positional args needed (host is looked up from the job)
		if strings.TrimSpace(runKillJobIDRaw) != "" {
			return nil
		}
		// An explicit `--` separator means every following token forms the
		// command (e.g. `weft run uv run x.py -- --flag`); the host comes only
		// from --host in that form, so any positional count is acceptable.
		dashed := cmd.ArgsLenAtDash() >= 0
		// --from mode: 0 args (copies from source job) or a command override.
		if strings.TrimSpace(runFromRaw) != "" {
			if len(args) > 1 && !dashed {
				return fmt.Errorf("--from accepts at most one positional argument (command override)")
			}
			return nil
		}
		if dashed {
			if len(args) == 0 {
				return fmt.Errorf("requires <command> argument")
			}
			return nil
		}
		// Normal mode: command, or the legacy/documented <host> <command> form.
		if len(args) == 1 {
			return nil
		}
		if len(args) == 2 && runHost == "" {
			return nil
		}
		if len(args) == 2 {
			return fmt.Errorf("cannot use both --host and positional host")
		}
		return fmt.Errorf("requires <command> argument")
	}),
	RunE: runRun,
}

var (
	runHost            string
	runDir             string
	runDescription     string
	runProject         string
	runDraft           bool
	runFollow          bool
	runWait            bool
	runNoWait          bool // explicit no-op flag for tooling compatibility
	runKillJobID       int64
	runKillJobIDRaw    string
	runFrom            int64
	runFromRaw         string
	runEnvVars         []string
	runTags            []string
	runAfter           int64
	runAfterRaw        string
	runAfterAny        int64
	runAfterAnyRaw     string
	runGPU             string
	runAffinity        []string
	runGPUCount        int
	runGPUMem          int
	runGPUMemStrict    bool
	runInterconnect    string
	runCPUCores        int
	runCPUMem          int
	runCPUMemStrict    bool
	runDiskGB          int
	runDiskMaxGB       int
	runRuntimeDiskGB   int
	runGPUClass        string
	runCUDADriverMin   string
	runProvider        string
	runRunpodCloudType string
	runMaxHourlyRate   string
	runMaxSpend        string
	runMaxTime         string
	runGracePeriod     string
	runMinSurvival     float64
	runInputs          []string
	runOutputs         []string
	runProduces        []string
	runNeeds           []string
	runDryRun          bool
	runNoSync          bool
	runIfOnline        bool
	runJSON            bool
	runAgent           string
	runCapabilities    []string
	runIdempotencyKey  string
	runHFToken         bool
	runHFTokenFrom     string
	runSecretVars      []string

	submitJobsToInstanceFunc = campaign.SubmitJobsToInstance
)

const defaultGPUMemGB = opsqueue.DefaultGPUMemGB

const (
	runCloudReuseAckWaitTimeout  = 250 * time.Millisecond
	runCloudReuseAckPollInterval = 50 * time.Millisecond
	runAutoPlacementWaitTimeout  = 2 * time.Second
	runAutoPlacementPollInterval = 100 * time.Millisecond
)

var validateRentalJobImageFunc = campaign.ValidateJobImageAvailability
var pinRunSourceSnapshotFunc = pinRunSourceSnapshot

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

func trySubmitJobToGraceReuse(cmd *cobra.Command, database *sql.DB, jobID int64) (bool, error) {
	job, err := db.GetJobByID(database, jobID)
	if err != nil {
		return false, fmt.Errorf("load queued job %s: %w", ids.FormatJobID(jobID), err)
	}
	if job == nil || !job.IsUnplacedQueued() {
		return false, nil
	}
	inst, matchedJob, err := findGraceRetryCandidate(database, job, time.Now())
	if err != nil {
		return false, err
	}
	if inst == nil {
		return false, nil
	}

	r2Client, err := newR2ClientFromConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: found warm instance %s from failed job %s, but cannot submit automatically: %v\n",
			ids.FormatInstanceID(inst.ID), ids.FormatJobID(matchedJob.ID), err)
		return false, nil
	}

	outcome, _, err := submitJobToCloudReuse(database, r2Client, inst.ID, job)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: found warm instance %s from failed job %s, but automatic submit failed: %v\n",
			ids.FormatInstanceID(inst.ID), ids.FormatJobID(matchedJob.ID), err)
		return false, nil
	}
	switch outcome {
	case cloudReuseAckReceived:
		if !runJSON {
			fmt.Fprintf(cmd.OutOrStdout(), "Job %s accepted and assigned to warm instance %s\n",
				ids.FormatJobID(jobID), ids.FormatInstanceID(inst.ID))
			printRunSubmissionExpectation(cmd.OutOrStdout(), database, jobID)
		}
		return true, nil
	case cloudReuseAckNotObserved:
		fmt.Fprintf(os.Stderr, "warning: warm instance %s did not acknowledge job %s before the short submit timeout; continuing with normal placement\n",
			ids.FormatInstanceID(inst.ID), ids.FormatJobID(jobID))
		return false, nil
	default:
		return false, nil
	}
}

func findGraceRetryCandidate(database *sql.DB, job *db.Job, now time.Time) (*db.Launch, *db.Job, error) {
	if job == nil || job.WorkingDir == "" || job.Command == "" {
		return nil, nil, nil
	}
	launches, err := db.ListLaunches(database)
	if err != nil {
		return nil, nil, fmt.Errorf("list launches: %w", err)
	}
	var bestLaunch *db.Launch
	var bestJob *db.Job
	for _, inst := range launches {
		if !launchInActiveGrace(inst, now) {
			continue
		}
		jobs, err := db.GetLaunchJobsIncludingAttempts(database, inst.ID)
		if err != nil {
			return nil, nil, fmt.Errorf("list jobs for %s: %w", ids.FormatInstanceID(inst.ID), err)
		}
		for _, prior := range jobs {
			if !matchesGraceRetryJob(job, prior) {
				continue
			}
			if bestLaunch == nil || graceCandidateNewer(inst, bestLaunch) {
				bestLaunch = inst
				bestJob = prior
			}
		}
	}
	return bestLaunch, bestJob, nil
}

func launchInActiveGrace(inst *db.Launch, now time.Time) bool {
	if inst == nil || inst.Status != db.LaunchStatusGrace {
		return false
	}
	if inst.GraceDeadline == nil {
		return true
	}
	return *inst.GraceDeadline > now.Unix()
}

func graceCandidateNewer(a, b *db.Launch) bool {
	if a == nil {
		return false
	}
	if b == nil {
		return true
	}
	aStarted, bStarted := int64(0), int64(0)
	if a.GraceStartedAt != nil {
		aStarted = *a.GraceStartedAt
	}
	if b.GraceStartedAt != nil {
		bStarted = *b.GraceStartedAt
	}
	if aStarted != bStarted {
		return aStarted > bStarted
	}
	return a.ID > b.ID
}

func matchesGraceRetryJob(job, prior *db.Job) bool {
	if job == nil || prior == nil {
		return false
	}
	if prior.Status != db.StatusFailed {
		return false
	}
	if job.WorkingDir == "" || prior.WorkingDir == "" || job.WorkingDir != prior.WorkingDir {
		return false
	}
	return sameRetryCommandShape(job.Command, prior.Command)
}

func sameRetryCommandShape(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	aScripts := normalizedPythonScripts(a)
	bScripts := normalizedPythonScripts(b)
	return len(aScripts) == 1 && len(bScripts) == 1 && aScripts[0] == bScripts[0]
}

func normalizedPythonScripts(command string) []string {
	scripts := dataloc.ExtractPythonScripts(command)
	for i, script := range scripts {
		scripts[i] = filepath.Clean(strings.Trim(script, `"'`))
	}
	return scripts
}

type draftRunParams struct {
	Config           *config.Config
	Host             string
	WorkingDir       string
	Command          string
	Description      string
	ProjectName      string
	EnvVars          []string
	Tags             []string
	GPU              string
	GPUClass         string
	GPUMemGB         *int
	GPUMemMaxGB      *int
	MaxComputeCap    string
	CLIOverrides     *db.CLIResourceOverrides
	Inputs           []string
	BestEffortInputs []string
	Outputs          []string
	OutputDirs       []string
	Produces         []string
	Needs            []string
	Disk             *db.JobDiskMetadata
	Source           *db.JobSourceMetadata
}

func recordDraftRunJob(cmd *cobra.Command, database *sql.DB, params draftRunParams) error {
	metadata := &db.JobMetadata{Source: params.Source}
	jobID, err := ops.RecordDraftJob(database, ops.QueueJobParams{
		Host:             params.Host,
		WorkingDir:       params.WorkingDir,
		Command:          params.Command,
		Description:      params.Description,
		Project:          params.ProjectName,
		EnvVars:          params.EnvVars,
		Tags:             params.Tags,
		GPU:              params.GPU,
		GPUClass:         params.GPUClass,
		GPUMemGB:         params.GPUMemGB,
		GPUMemMaxGB:      params.GPUMemMaxGB,
		Inputs:           params.Inputs,
		BestEffortInputs: params.BestEffortInputs,
		Outputs:          params.Outputs,
		OutputDirs:       params.OutputDirs,
		Produces:         params.Produces,
		Needs:            params.Needs,
		Disk:             params.Disk,
		Metadata:         metadata,
		CLIOverrides:     params.CLIOverrides,
		MaxComputeCap:    params.MaxComputeCap,
		SubmitterSession: submitterSession(),
	})
	if err != nil {
		return fmt.Errorf("record draft job: %w", err)
	}
	backend := ""
	if params.Config != nil && params.Host != "" {
		backend = params.Config.HostBackend(params.Host)
	}
	if err := db.SetJobBackend(database, jobID, backend); err != nil {
		return fmt.Errorf("set job backend: %w", err)
	}

	w := cmd.OutOrStdout()
	if params.Host != "" {
		fmt.Fprintf(w, "Draft job #%d saved for %s\n\n", jobID, params.Host)
	} else {
		fmt.Fprintf(w, "Draft job #%d saved\n\n", jobID)
	}
	fmt.Fprintf(w, "  Working dir: %s\n", params.WorkingDir)
	fmt.Fprintf(w, "  Command: %s\n", params.Command)
	if params.Description != "" {
		fmt.Fprintf(w, "  Description: %s\n", params.Description)
	}
	return nil
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
	runCmd.Flags().StringVar(&runKillJobIDRaw, "kill", "", "Kill a job by ID (synonym for 'weft kill')")
	runCmd.Flags().StringVar(&runFromRaw, "from", "", "Copy settings from existing job ID before running")
	runCmd.Flags().StringSliceVarP(&runEnvVars, "env", "e", nil, "Environment variable (VAR=value), can be repeated")
	addSecretEnvFlags(runCmd, &runHFToken, &runHFTokenFrom, &runSecretVars)
	runCmd.Flags().StringSliceVar(&runTags, "tag", nil, "Tag to attach to the job (can be repeated). Reserved tags: 'exclusive' runs alone; 'benchmark-isolation' waits for system-wide idle; 'rental' skips local placement; 'inventory' blocks rental placement; 'interruptible' allows interruptible cloud placement ('preemptible' is accepted as a synonym)")
	runCmd.Flags().StringVar(&runAfterRaw, "after", "", "Start job after another job succeeds (implies --queue)")
	runCmd.Flags().StringVar(&runAfterRaw, "depends-on", "", "Alias for --after; start job after another job succeeds (implies --queue)")
	runCmd.Flags().StringVar(&runAfterAnyRaw, "after-any", "", "Start job after another job completes, success or failure (implies --queue)")
	runCmd.Flags().StringArrayVar(&runAffinity, "affinity", nil, "Pin this job to the Vast.ai physical machine of a machine ID, instance (wi...), or job (wj...); repeatable and comma-separated")
	runCmd.Flags().StringVar(&runGPU, "gpu", "", "GPU constraint: class, generation, or family (e.g., a100, ampere+, nvidia); append >=NGB for memory (e.g., nvidia>=24GB)")
	runCmd.Flags().IntVar(&runGPUCount, "gpus", 0, "Exact number of GPUs to expose on one host or rental instance")
	runCmd.Flags().IntVar(&runGPUMem, "gpu-mem", 0, "GPU memory reservation in GB per device (default: 20 when GPU is used)")
	runCmd.Flags().BoolVar(&runGPUMemStrict, "gpu-mem-strict", false, "Use exact gpu-mem matching without default safety headroom")
	runCmd.Flags().StringVar(&runInterconnect, "interconnect", "", "Multi-GPU interconnect requirement: any, pcie, nvlink (present), or nvlink-uniform (every GPU pair)")
	runCmd.Flags().Bool("nvlink-required", false, "Alias for --interconnect=nvlink")
	runCmd.Flags().Bool("same-host", false, "Require all requested GPUs on one host (default for --gpus)")
	runCmd.Flags().IntVar(&runCPUCores, "cpu-cores", 0, "Minimum effective CPU cores/vCPUs for rental placement")
	runCmd.Flags().IntVar(&runCPUMem, "cpu-mem", 0, "Minimum host/system RAM in GB; filters rental offers and gates on-prem hosts")
	runCmd.Flags().BoolVar(&runCPUMemStrict, "cpu-mem-strict", false, "Use exact cpu-mem matching without default safety headroom")
	runCmd.Flags().IntVar(&runDiskGB, "disk", 0, "Rental instance disk floor in GB")
	runCmd.Flags().IntVar(&runRuntimeDiskGB, "runtime-disk", 0, "Extra rental scratch/cache disk headroom in GB")
	runCmd.Flags().IntVar(&runDiskMaxGB, "disk-max", 0, "Cap the estimated rental disk at this many GB (may lower the request below the estimate)")
	runCmd.Flags().StringVar(&runGPUClass, "gpu-class", "", "GPU class or generation (e.g., a100, ampere, ampere+); '+' means that generation or newer")
	runCmd.Flags().StringVar(&runCUDADriverMin, "cuda-driver-min", "", "Minimum NVIDIA driver CUDA support required (e.g., 12.4, 12.8, hopper, blackwell). Filters cloud offers by cuda_max_good; the parallel PEP 723 key is `min-cuda` / `cuda-driver-min`")
	runCmd.Flags().StringVar(&runCUDADriverMin, "min-cuda", "", "Alias for --cuda-driver-min")
	_ = runCmd.Flags().MarkHidden("min-cuda")
	runCmd.Flags().StringVar(&runProvider, "provider", "", "Cloud provider for rental placement (vastai or runpod)")
	runCmd.Flags().StringVar(&runRunpodCloudType, "runpod-cloud-type", "", "RunPod cloud type for rental placement: community or secure")
	runCmd.Flags().StringVar(&runMaxHourlyRate, "max-hourly-rate", "", "Maximum rental offer rate in USD per hour; 0 clears")
	runCmd.Flags().StringVar(&runMaxSpend, "max-spend", "", "Maximum total rental spend in USD; 0 clears")
	runCmd.Flags().StringVar(&runMaxTime, "max-time", "", "Maximum rental lifetime (for example, 3h); 0 clears")
	runCmd.Flags().StringVar(&runGracePeriod, "grace-period", "", "Keep a failed rental alive for this duration; 0 disables, default clears")
	runCmd.Flags().Float64Var(&runMinSurvival, "min-survival", 0.4, "Minimum Weft learned end-to-end survival probability (0-1; 0 disables; distinct from provider reliability)")
	runCmd.Flags().BoolVar(&runWait, "wait", false, "Wait for job to complete before returning")
	runCmd.Flags().BoolVar(&runNoWait, "no-wait", false, "Don't wait for job (default behavior, for explicit acknowledgment)")
	runCmd.Flags().StringSliceVar(&runInputs, "input", nil, "Input data asset (e.g., hf:EleutherAI/pythia-160m), can be repeated")
	runCmd.Flags().StringSliceVar(&runOutputs, "output", nil, "Output data asset (e.g., checkpoint:llama-ft-v1), can be repeated")
	runCmd.Flags().StringSliceVar(&runProduces, "produces", nil, "Artifact path this job produces (repeatable, e.g., output/model.pt or output/model.pt:100)")
	runCmd.Flags().StringSliceVar(&runNeeds, "needs", nil, "Artifact path:version this job needs (repeatable, e.g., output/model.pt:100)")
	runCmd.Flags().BoolVar(&runDryRun, "dry-run", false, "Show placement scores without submitting the job")
	runCmd.Flags().BoolVar(&runNoSync, "no-sync", false, "Skip source sync before submission")
	runCmd.Flags().BoolVar(&runIfOnline, "if-online", false, "Submit immediately to an online inventory host, or create no job")
	runCmd.Flags().BoolVar(&runIfOnline, "no-queue", false, "Alias for --if-online")
	runCmd.Flags().BoolVar(&runJSON, "json", false, "Print a versioned JSON submission receipt")
	runCmd.Flags().StringVar(&runAgent, "agent", "", "Require an authenticated agent CLI on the host (claude, codex, gemini, opencode, or kimi)")
	runCmd.Flags().StringSliceVar(&runCapabilities, "require-capability", nil, "Require a host capability label; can be repeated")
	runCmd.Flags().StringVar(&runIdempotencyKey, "idempotency-key", "", "Caller assignment ID used to deduplicate submission retries")
	addJobAddFlagAliases(runCmd)
}

var recordQueuedJobMutationFunc = localmutate.RecordQueuedJob

func recordQueuedJobSingleWriter(database *sql.DB, params ops.QueueJobParams) (int64, error) {
	return recordQueuedJobMutationFunc(context.Background(), database, params)
}

func parseRunJobIDFlags() error {
	var err error
	if runKillJobID, err = parseOptionalJobIDFlag("kill", runKillJobIDRaw); err != nil {
		return err
	}
	if runFrom, err = parseOptionalJobIDFlag("from", runFromRaw); err != nil {
		return err
	}
	if runAfter, err = parseOptionalJobIDFlag("after", runAfterRaw); err != nil {
		return err
	}
	if runAfterAny, err = parseOptionalJobIDFlag("after-any", runAfterAnyRaw); err != nil {
		return err
	}
	return nil
}

func runRun(cmd *cobra.Command, args []string) error {
	if err := parseRunJobIDFlags(); err != nil {
		return err
	}
	if runIfOnline && (runDraft || runAfter > 0 || runAfterAny > 0 || runNoSync) {
		return fmt.Errorf("--if-online cannot be combined with --draft, dependencies, or --no-sync")
	}
	if runJSON && (runDraft || runDryRun || runWait || runFollow || runAfter > 0 || runAfterAny > 0 || runKillJobID > 0) {
		return fmt.Errorf("--json is supported for direct submissions; it cannot be combined with draft, dry-run, wait/follow, dependency, or kill modes")
	}
	if key := strings.TrimSpace(runIdempotencyKey); strings.ContainsAny(key, "\r\n") || len(key) > 200 {
		return fmt.Errorf("--idempotency-key must be at most 200 characters and contain no newlines")
	}

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
	rentalPolicyFlags, err := parseRentalPolicyFlags(
		cmd, runMaxHourlyRate, runMaxSpend, runMaxTime, runGracePeriod, runMinSurvival,
	)
	if err != nil {
		return err
	}

	// Open database early for --from support
	database, err := db.Open()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	var host, command string
	// All --needs specs flow into jobs.needs and are classified later at
	// placement/launch time (see internal/campaign/needs_classify.go).
	resolvedNeeds := append([]string(nil), runNeeds...)

	// --host flag takes priority
	host = runHost

	var fromJob *db.Job
	// Handle --from mode: copy settings from existing job
	if runFrom > 0 {
		fromJob, err = db.GetJobByID(database, runFrom)
		if err != nil {
			return fmt.Errorf("get job %s: %w", ids.FormatJobID(runFrom), err)
		}
		if fromJob == nil {
			return fmt.Errorf("job %s not found", ids.FormatJobID(runFrom))
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
		if runGPUCount == 0 {
			if count := fromJob.RequestedGPUCount(); count > 1 {
				runGPUCount = count
			}
		}
		if runProvider == "" {
			if provider, ok := db.RequestedProvider(fromJob.Tags); ok {
				runProvider = provider
			}
		}
		if !cmd.Flags().Changed("runpod-cloud-type") && runRunpodCloudType == "" {
			runRunpodCloudType = fromJob.RequestedRunpodCloudType()
		}
		if runGPUMem == 0 && fromJob.GPUMemGB != nil {
			runGPUMem = *fromJob.GPUMemGB
		}
		if runInterconnect == "" {
			runInterconnect = fromJob.RequestedInterconnect()
		}
		if runCPUCores == 0 {
			runCPUCores = fromJob.RequestedCPUCores()
		}
		if runCPUMem == 0 && fromJob.CLIResourceOverrides != nil && fromJob.CLIResourceOverrides.CPUMemGB != nil {
			runCPUMem = *fromJob.CLIResourceOverrides.CPUMemGB
			if fromJob.CLIResourceOverrides.CPUMemStrict != nil {
				runCPUMemStrict = *fromJob.CLIResourceOverrides.CPUMemStrict
			}
		}
		if fromJob.Metadata != nil && fromJob.Metadata.Disk != nil {
			if runDiskGB == 0 {
				runDiskGB = fromJob.Metadata.Disk.DiskGB
			}
			if runRuntimeDiskGB == 0 {
				runRuntimeDiskGB = fromJob.Metadata.Disk.RuntimeDiskGB
			}
			if runDiskMaxGB == 0 {
				runDiskMaxGB = fromJob.Metadata.Disk.DiskMaxGB
			}
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
		if len(runCapabilities) == 0 && runAgent == "" && fromJob.Metadata != nil && fromJob.Metadata.Agent != nil {
			runCapabilities = append([]string(nil), fromJob.Metadata.Agent.RequiredCapabilities...)
		}

		// Allow overriding command from positional args. Joining supports the
		// passthrough form `weft run --from N -- python eval.py --flag`.
		if len(args) > 0 {
			command = strings.Join(args, " ")
		}
	} else {
		// Parse positional args. An explicit `--` separator means all
		// positionals form the command and the host comes only from --host
		// (e.g. `weft run uv run x.py -- --models gpt2`). Without `--`,
		// preserve the single-command form and the legacy `<host> <command>`
		// two-arg form.
		if cmd.ArgsLenAtDash() >= 0 {
			command = strings.Join(args, " ")
		} else if len(args) == 1 {
			command = args[0]
		} else if len(args) == 2 && host == "" {
			host = args[0]
			command = args[1]
		}
	}

	// Validate command against blocked patterns
	cfg, _ := config.Load()
	setUsageHintsFromConfig(cfg)
	if err := cfg.ValidateCommand(command); err != nil {
		return err
	}
	// Phase recorder: attribute wall-clock to predictor/placement/submit/sync
	// so users (and agents) can see where time went instead of guessing.
	// See cmd/run_progress.go.
	rec := newRunPhaseRecorder(cmd.ErrOrStderr(), command)
	defer rec.PrintSummary()

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

	// Fail fast when the command directly execs a local script that lacks the
	// +x bit; otherwise the job dies at runtime with exit 126 "Permission denied".
	if err := dataloc.CheckBareScriptExecutable(localDir, command); err != nil {
		return err
	}
	warnPEP723ScriptEnvironmentMismatch(cmd.ErrOrStderr(), localDir, command)

	outputDirs := config.ProjectOutputDirs(localDir)

	// Merge project-level inputs with CLI --input flags
	originalRunInputs := append([]string(nil), runInputs...)
	projectInputs := config.ProjectInputs(localDir)
	runInputs = mergeDedup(projectInputs, runInputs)

	// Apply PEP 723 [tool.weft] script metadata as defaults (CLI flags take precedence).
	scriptMeta, scriptMetaErr := scanRunScriptMeta(localDir, command)
	if scriptMetaErr != nil {
		return scriptMetaErr
	}
	if scriptMeta != nil && len(scriptMeta.Inputs) > 0 {
		runInputs = mergeDedup(runInputs, scriptMeta.Inputs)
	}
	explicitInputs := mergeDedup(mergeDedup(projectInputs, originalRunInputs), scriptMetaInputs(scriptMeta))

	autoDetectedInputs := autoDetectedInputsForCommand(localDir, command, explicitInputs)
	runInputs = mergeDedup(runInputs, autoDetectedInputs)
	if len(autoDetectedInputs) > 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "Auto-detected inputs: %s\n", strings.Join(autoDetectedInputs, ", "))
	}

	// Capture CLI intent for resource flags BEFORE script defaults are merged in.
	// Resolve at submit rather than at placement: the reference forms are wi/wj
	// IDs whose machine is knowable now, and a job that names a machine weft
	// cannot resolve should fail here rather than sit unplaceable later.
	var affinityMachines []string
	if len(runAffinity) > 0 {
		database, err := db.Open()
		if err != nil {
			return fmt.Errorf("open database: %w", err)
		}
		resolved, warnings, err := db.ResolveAffinityMachineIDs(database, runAffinity)
		database.Close()
		if err != nil {
			return fmt.Errorf("resolve --affinity: %w", err)
		}
		for _, w := range warnings {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}
		if len(resolved) == 0 {
			return fmt.Errorf("--affinity did not resolve to any machine_id")
		}
		affinityMachines = resolved
	}

	// These overrides are persisted on the job so retries can replay the user's
	// original submission intent against updated script metadata.
	cliOverrides := &db.CLIResourceOverrides{}
	if fromJob != nil {
		inheritRentalPolicyOverrides(cliOverrides, fromJob.CLIResourceOverrides)
	}
	applyRentalPolicyFlags(cmd, cliOverrides, rentalPolicyFlags)
	if strings.TrimSpace(host) != "" {
		cliOverrides.Host = strings.TrimSpace(host)
	}
	if runGPU != "" {
		cliOverrides.GPU = runGPU
	}
	if runGPUClass != "" {
		cliOverrides.GPUClass = runGPUClass
	}
	if runGPUCount != 0 {
		count := runGPUCount
		cliOverrides.GPUCount = &count
	}
	if runGPUMem != 0 {
		mem := runGPUMem
		cliOverrides.GPUMemGB = &mem
	}
	if cmd.Flags().Changed("gpu-mem-strict") {
		s := runGPUMemStrict
		cliOverrides.GPUMemStrict = &s
	}
	if runInterconnect != "" {
		cliOverrides.Interconnect = runInterconnect
	}
	if runCPUCores != 0 {
		cores := runCPUCores
		cliOverrides.CPUCores = &cores
	}
	if cmd.Flags().Changed("disk") {
		disk := runDiskGB
		cliOverrides.DiskGB = &disk
	}
	if cmd.Flags().Changed("runtime-disk") {
		runtimeDisk := runRuntimeDiskGB
		cliOverrides.RuntimeDiskGB = &runtimeDisk
	}
	if len(affinityMachines) > 0 {
		cliOverrides.MachineAffinity = affinityMachines
	}
	if cmd.Flags().Changed("disk-max") {
		diskMax := runDiskMaxGB
		cliOverrides.DiskMaxGB = &diskMax
	}
	if runCUDADriverMin != "" {
		canonical, parseErr := placement.ParseCUDADriverFloor(runCUDADriverMin)
		if parseErr != nil {
			return fmt.Errorf("--cuda-driver-min: %w", parseErr)
		}
		if canonical == "" {
			// "any"/"none" clears the inferred floor. Persist the sentinel
			// so daemon-side re-derivation (ConstraintsFromJob, the cloud
			// planner) honors the clear, not just this submit invocation.
			cliOverrides.MinCUDAVersion = "any"
		} else {
			cliOverrides.MinCUDAVersion = canonical
		}
	}
	submitToken := "run-" + uuid.NewString()
	submissionNonce := ""
	if runIdempotencyKey = strings.TrimSpace(runIdempotencyKey); runIdempotencyKey != "" {
		submitToken = "external:" + runIdempotencyKey
		submissionNonce = uuid.NewString()
	}
	runSubmitterSession := submitterSession()
	requiredCapabilities, err := normalizeRunCapabilities(runAgent, runCapabilities)
	if err != nil {
		return err
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
		if !cmd.Flags().Changed("gpus") && runGPUCount == 0 && meta.GPUCount > 0 {
			runGPUCount = meta.GPUCount
			applied = append(applied, fmt.Sprintf("gpus=%d", meta.GPUCount))
		}
		if runGPUMem == 0 && meta.GPUMemGB > 0 {
			runGPUMem = meta.GPUMemGB
			applied = append(applied, fmt.Sprintf("gpu-mem=%dGB", meta.GPUMemGB))
		}
		if !cmd.Flags().Changed("gpu-mem-strict") && meta.GPUMemStrict != nil {
			runGPUMemStrict = *meta.GPUMemStrict
			applied = append(applied, fmt.Sprintf("gpu-mem-strict=%t", runGPUMemStrict))
		}
		if !cmd.Flags().Changed("interconnect") && !cmd.Flags().Changed("nvlink-required") && runInterconnect == "" && meta.Interconnect != "" {
			runInterconnect = meta.Interconnect
			applied = append(applied, fmt.Sprintf("interconnect=%s", meta.Interconnect))
		}
		if !cmd.Flags().Changed("cpu-cores") && runCPUCores == 0 && meta.CPUCores > 0 {
			runCPUCores = meta.CPUCores
			applied = append(applied, fmt.Sprintf("cpu-cores=%d", meta.CPUCores))
		}
		if !cmd.Flags().Changed("cpu-mem") && runCPUMem == 0 && meta.CPUMemGB > 0 {
			runCPUMem = meta.CPUMemGB
			applied = append(applied, fmt.Sprintf("cpu-mem=%dGB", meta.CPUMemGB))
		}
		if !cmd.Flags().Changed("cpu-mem-strict") && meta.CPUMemStrict != nil {
			runCPUMemStrict = *meta.CPUMemStrict
			applied = append(applied, fmt.Sprintf("cpu-mem-strict=%t", runCPUMemStrict))
		}
		if !cmd.Flags().Changed("disk") && runDiskGB == 0 && meta.DiskGB > 0 {
			runDiskGB = meta.DiskGB
			applied = append(applied, fmt.Sprintf("disk=%dGB", meta.DiskGB))
		}
		if !cmd.Flags().Changed("runtime-disk") && runRuntimeDiskGB == 0 && meta.RuntimeDiskGB > 0 {
			runRuntimeDiskGB = meta.RuntimeDiskGB
			applied = append(applied, fmt.Sprintf("runtime-disk=%dGB", meta.RuntimeDiskGB))
		}
		if !cmd.Flags().Changed("disk-max") && runDiskMaxGB == 0 && meta.DiskMaxGB > 0 {
			runDiskMaxGB = meta.DiskMaxGB
			applied = append(applied, fmt.Sprintf("disk-max=%dGB", meta.DiskMaxGB))
		}
		if len(meta.Inputs) > 0 {
			runInputs = mergeDedup(runInputs, meta.Inputs)
			explicitInputs = mergeDedup(explicitInputs, meta.Inputs)
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
			runTags = mergeDedup(runTags, []string{db.TagInterruptible})
			applied = append(applied, "interruptible=true")
		}
		if !cmd.Flags().Changed("runpod-cloud-type") && runRunpodCloudType == "" && meta.RunpodCloudType != "" {
			runRunpodCloudType = meta.RunpodCloudType
			applied = append(applied, fmt.Sprintf("runpod-cloud-type=%s", meta.RunpodCloudType))
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
		if meta.HFOffline != nil && *meta.HFOffline {
			// Opt-in: run with HF offline so a fully-staged job never touches
			// the network. Declared inputs are pre-staged (and the staging gate
			// blocks jobs whose declared inputs aren't available); the user
			// asserts the job loads no undeclared HF assets at runtime.
			runEnvVars = append(runEnvVars, hfOfflineEnvVars(runInputs)...)
			applied = append(applied, "hf-offline=true")
		}
		if len(applied) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "Script metadata: %s\n", strings.Join(applied, ", "))
		}
	}

	// Prefer hf-dataset:X over a same-ID hf:X: the former is the authoritative
	// form (code-scanned from load_dataset or explicit), the latter is almost
	// always user error since datasets don't exist as HF models.
	if deduped, removed := dropModelRefsShadowedByDatasetRefs(runInputs); len(removed) > 0 {
		runInputs = deduped
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Warning: dropping %s — also declared as hf-dataset. Use hf-dataset:<id> for HF datasets.\n",
			strings.Join(removed, ", "))
	}
	if normalized, corrected := dataloc.NormalizeMisprefixedHFDatasets(runInputs); len(corrected) > 0 {
		runInputs = normalized
		fmt.Fprintf(cmd.ErrOrStderr(),
			"Warning: %s declared as hf: but resolves to an HF dataset; staging as hf-dataset:. Declare hf-dataset:<id> to silence this.\n",
			strings.Join(corrected, ", "))
	}
	bestEffortInputs := bestEffortAutoDetectedInputs(runInputs, autoDetectedInputs, explicitInputs)
	if err := dataloc.ValidateExplicitHFInputs(runInputs, bestEffortInputs); err != nil {
		return fmt.Errorf("--input: %w", err)
	}
	runEnvVars, err = applySecretEnv(runEnvVars, runInputs, runHFToken, runHFTokenFrom, runSecretVars)
	if err != nil {
		return err
	}
	var sourceMeta *db.JobSourceMetadata
	if runDryRun {
		sourceMeta, err = buildJobSourceMetadata(localDir, runInputs, []string{command})
		if err != nil {
			return err
		}
	}
	// Warn when the job appears torch-using and cloud-bound but the lockfile
	// has no torch pin to derive a driver floor from. Without a pin, weft
	// can't tell whether the rental's driver will satisfy the eventual
	// `uv sync` (or an in-script `uv venv` that resolves torch on-instance).
	// Most likely failure is `cuda_driver_too_old` 2-3 minutes into the run.
	// Only jobs that request a GPU consume a GPU rental where a driver floor
	// matters; a no-GPU job is CPU-placed, so the torch driver-floor gate has
	// nothing to protect (runGPU/runGPUClass/runGPUMem/runGPUCount already
	// reflect CLI flags and PEP 723 [tool.weft] GPU metadata by this point).
	gpuRequested := runGPU != "" || runGPUClass != "" || runGPUMem > 0 || runGPUCount > 0
	if err := rejectUnlockedTorchCloudRuntime(localDir, command, host, runCUDADriverMin, runTags, scriptMeta, gpuRequested); err != nil {
		return err
	}
	maybeWarnNoTorchPinForCloud(cmd, localDir, command, host, runCUDADriverMin, scriptMeta, gpuRequested)

	// Print recommendations for common patterns
	printCommandRecommendations(command, localDir)

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
	if cmd.Flags().Changed("nvlink-required") {
		required, _ := cmd.Flags().GetBool("nvlink-required")
		if required {
			if runInterconnect != "" && !strings.EqualFold(runInterconnect, placement.InterconnectNVLink) {
				return fmt.Errorf("--nvlink-required cannot be combined with --interconnect=%s", runInterconnect)
			}
			runInterconnect = placement.InterconnectNVLink
		}
	}
	if cmd.Flags().Changed("same-host") {
		sameHost, _ := cmd.Flags().GetBool("same-host")
		if !sameHost && runGPUCount > 1 {
			return fmt.Errorf("--same-host=false is not supported; multi-GPU requests are single-host only")
		}
	}
	if runGPUCount < 0 {
		return fmt.Errorf("--gpus must be >= 1")
	}
	if runCPUCores < 0 {
		return fmt.Errorf("--cpu-cores must be >= 1")
	}
	if runCPUMem < 0 {
		return fmt.Errorf("--cpu-mem must be >= 1")
	}
	var normalizeErr error
	runInterconnect, normalizeErr = normalizeInterconnect(runInterconnect)
	if normalizeErr != nil {
		return normalizeErr
	}
	if runGPUCount > 1 && runInterconnect == "" {
		runInterconnect = placement.InterconnectAny
	}
	if runGPUCount > 0 {
		count := runGPUCount
		cliOverrides.GPUCount = &count
	}
	if runInterconnect != "" {
		cliOverrides.Interconnect = runInterconnect
	}
	if runCPUCores > 0 {
		cores := runCPUCores
		cliOverrides.CPUCores = &cores
	}
	if runCPUMem > 0 {
		mem := runCPUMem
		cliOverrides.CPUMemGB = &mem
		if runCPUMemStrict {
			strict := true
			cliOverrides.CPUMemStrict = &strict
		}
	}
	providerFlagChanged := cmd.Flags().Changed("provider")
	if providerFlagChanged {
		normalizedProvider, providerErr := normalizeProviderFlag(runProvider)
		if providerErr != nil {
			return fmt.Errorf("--provider: %w", providerErr)
		}
		runProvider = normalizedProvider
		runTags, err = withProviderTag(runTags, runProvider)
		if err != nil {
			return fmt.Errorf("--provider: %w", err)
		}
	}
	if cmd.Flags().Changed("runpod-cloud-type") || strings.TrimSpace(runRunpodCloudType) != "" {
		normalizedCloudType, cloudTypeErr := normalizeRunpodCloudTypeFlag(runRunpodCloudType)
		if cloudTypeErr != nil {
			return fmt.Errorf("--runpod-cloud-type: %w", cloudTypeErr)
		}
		runRunpodCloudType = normalizedCloudType
		if runRunpodCloudType != "" {
			runTags, err = applyRunpodCloudTypeProviderIntent(runTags, host, runRunpodCloudType)
			if err != nil {
				return err
			}
			cliOverrides.RunpodCloudType = runRunpodCloudType
		}
	}

	gpuMemHardwareFloor := false
	// Resolve --gpu into --gpu-class (and optionally a hardware memory floor).
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
			gpuMemHardwareFloor = true
		}
	}
	if err := validateGPUSKUMemory(runGPUClass); err != nil {
		return err
	}

	// --input participates in placement, while --needs drives staging.
	runNeeds = appendNamedAssetInputNeeds(runInputs, runNeeds)

	// Validate --needs entries have valid path:version or asset:NAME format
	if err := runner.ValidateNeedsSpecs(runNeeds); err != nil {
		return fmt.Errorf("--needs: %w", err)
	}
	if err := campaign.ValidateCUDADriverMinOverride(cliOverrides.MinCUDAVersion); err != nil {
		return err
	}
	if len(runNeeds) > 0 {
		resolvedHost, allNeeds, err := resolveArtifactNeedsPlacement(database, runNeeds, host)
		if err != nil {
			return fmt.Errorf("--needs: %w", err)
		}
		host = resolvedHost
		resolvedNeeds = allNeeds
	}
	requestedProvider, hasRequestedProvider := db.RequestedProvider(runTags)
	if runIfOnline && (hasRequestedProvider || db.HasRentalTag(runTags) || db.IsLaunchHost(host)) {
		if runJSON {
			_ = emitRunReceipt(cmd, runSubmissionReceipt{
				PlacementDecision: "not_accepted",
				SelectedHost:      host,
				IdempotencyKey:    runIdempotencyKey,
			})
		}
		return fmt.Errorf("--if-online admits inventory hosts only; no job was created")
	}
	if hasRequestedProvider && host != "" && !db.IsLaunchHost(host) {
		return fmt.Errorf("--provider=%s cannot be used with inventory host %q; omit --host to keep the job unplaced for rental launch", requestedProvider, host)
	}
	fastAutoSubmit := host == "" && !runIfOnline && !runDraft && !runDryRun && !runWait && !runFollow && runAfter == 0 && runAfterAny == 0
	// Skip predictor entirely when placement won't use it: explicit --host,
	// rental-tagged jobs, draft submissions, and the fast auto-submit path.
	// The daemon/autopilot performs predictor-backed placement after the job
	// is durably recorded.
	predictorNeeded := host == "" && !db.HasRentalTag(runTags) && !runDraft && !fastAutoSubmit
	if predictorNeeded {
		endPredictor := rec.Phase("predictor", "checking predictor")
		err := ensurePredictorUsableFunc(cmd, cfg, "placement prediction")
		endPredictor()
		if err != nil {
			// Predictor is an optimization, never a gate: degrade to
			// heuristic estimates rather than blocking submission
			// (invariant PredictorNeverBlocksPlacement). Set
			// predictor.enabled = false to silence.
			fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %v; using heuristic estimates\n", err)
		}
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
	resolvedGPUMemGB, resolvedGPUMemMaxGB, _ := resolveEffectiveGPUMemAndCeiling(gpuMemCfg, intPtrOrNil(runGPUMem), gpu, gpuClass, runGPUMemStrict || gpuMemHardwareFloor, host, projectName, command, oomFloor)
	diskMeta := buildDiskMetadata(runDiskGB, runDiskMaxGB, runRuntimeDiskGB)

	// Placement scoring (used for auto-placement and dry-run)
	gpuMemGB := 0
	if resolvedGPUMemGB != nil {
		gpuMemGB = *resolvedGPUMemGB
	}
	resolvedConstraints, err := placement.ResolveConstraints(placement.ConstraintSource{
		GPUClass:             gpuClass,
		Provider:             requestedProvider,
		NumGPUs:              runGPUCount,
		GPUMemGB:             gpuMemGB,
		CPUCores:             runCPUCores,
		CPUMemGB:             db.EffectiveCPUMemGB(runCPUMem, runCPUMemStrict),
		Interconnect:         runInterconnect,
		Inputs:               runInputs,
		Command:              command,
		Project:              projectName,
		Tags:                 runTags,
		RequiredCapabilities: requiredCapabilities,
		LocalDir:             localDir,
		CLIOverrides:         cliOverrides,
	})
	if err != nil {
		return err
	}
	placementConstraints := resolvedConstraints.Constraints
	persistMaxComputeCap := resolvedConstraints.MaxComputeCapForPersistence
	// Tip placement toward producers' live rental instances so --needs
	// consumers co-locate with their producers and can read outputs from
	// the shared workdir (the classifier in internal/campaign/
	// needs_classify.go does the actual routing at launch time).
	placementConstraints.PreferredInstanceIDs = campaign.PreferredInstanceIDsFromNeeds(database, resolvedNeeds)

	if runIdempotencyKey != "" {
		if existingID, ok, lookupErr := db.FindJobIDBySubmitToken(database, submitToken); lookupErr != nil {
			return fmt.Errorf("look up idempotency key: %w", lookupErr)
		} else if ok {
			existing, getErr := db.GetJobByID(database, existingID)
			if getErr != nil {
				return fmt.Errorf("read idempotent submission: %w", getErr)
			}
			if runJSON {
				return emitRunReceipt(cmd, runReceiptForJob(existing, "deduplicated", false, true, runIdempotencyKey))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Job %s already exists for idempotency key %q\n", ids.FormatJobID(existingID), runIdempotencyKey)
			return nil
		}
	}

	if shouldValidateRentalJobImage(host, runTags, runDraft, runDryRun) {
		endImageProbe := rec.Phase("submit", "validating container image")
		ctx, cancel := context.WithTimeout(context.Background(), campaign.ImageProbeTimeout)
		err := validateRentalJobImageFunc(ctx, cfg, localDir, command)
		cancel()
		endImageProbe()
		if err != nil {
			return err
		}
	}

	// Build predictor closure if configured and used by this path.
	var predict placement.JobPredictor
	if predictorNeeded {
		predict = placement.BuildJobPredictorFromConfig(cfg, placementConstraints)
	}

	if runDryRun {
		endPlacement := rec.Phase("placement", "scoring hosts")
		scores, err := placement.ScoreHostsWithPredictor(database, placementConstraints, nil, predict)
		endPlacement()
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
		printSourceRootsPreview(cmd.OutOrStdout(), sourceMeta, remoteSourceRootForPreview(workingDir, ""))
		return nil
	}

	endSourcePin := rec.Phase("source", "pinning source snapshot")
	sourceMeta, err = pinRunSourceSnapshotFunc(context.Background(), localDir, runInputs, []string{command})
	endSourcePin()
	if err != nil {
		return fmt.Errorf("pin source snapshot: %w", err)
	}
	var agentMetadata *db.JobAgentMetadata
	if len(requiredCapabilities) > 0 {
		agentMetadata = &db.JobAgentMetadata{RequiredCapabilities: requiredCapabilities}
	}

	buildRunQueueParams := func(targetHost string) ops.QueueJobParams {
		return ops.QueueJobParams{
			Host:             targetHost,
			WorkingDir:       workingDir,
			Command:          command,
			Description:      runDescription,
			Project:          projectName,
			EnvVars:          runEnvVars,
			Tags:             runTags,
			GPUClass:         gpuClass,
			GPUMemGB:         resolvedGPUMemGB,
			GPUMemMaxGB:      resolvedGPUMemMaxGB,
			DepSpec:          encodeQueueDependencies(buildRunDependencies()),
			Inputs:           runInputs,
			BestEffortInputs: bestEffortInputs,
			Outputs:          runOutputs,
			OutputDirs:       outputDirs,
			Produces:         runProduces,
			Needs:            resolvedNeeds,
			Disk:             diskMeta,
			Metadata: &db.JobMetadata{
				Source:          sourceMeta,
				Agent:           agentMetadata,
				SubmissionNonce: submissionNonce,
			},
			CLIOverrides:     cliOverrides,
			MaxComputeCap:    persistMaxComputeCap,
			SubmitToken:      submitToken,
			SubmitterSession: runSubmitterSession,
		}
	}

	// Route through local placement for non-draft, non-dependency submissions.
	// Auto-placement records jobs first and lets the daemon/autopilot do slow
	// live probes and dispatch. It may choose an on-prem host from recent DB
	// observations, but never contacts SSH on the submit path.
	if !runDraft && runAfter == 0 && runAfterAny == 0 {
		var placementResult *placement.PlacementResult
		var placementPlan *placement.PlacementPlan
		autoPlacementPendingReason := blockreason.ReasonPlacementPending
		autoPlacedFromDB := false

		if host == "" {
			if fastAutoSubmit {
				endPlacement := rec.Phase("placement", "checking recent host state")
				plan, recent, err := evaluateRecentOnPremPlacement(database, placementConstraints)
				endPlacement()
				if err != nil {
					return err
				}
				autoPlacementPendingReason = fastSubmitPendingReasonForRecentOnPrem(autoPlacementPendingReason, runTags, recent, plan)
				if recent && plan != nil {
					placementPlan = plan
					pick := plan.Fast
					if pick == nil {
						pick = plan.Cheap
					}
					if pick != nil && pick.Kind == placement.CandidateOnPrem && pick.OnPrem != nil {
						placementResult = pick.OnPrem
						host = pick.OnPrem.Host
						autoPlacedFromDB = true
						oplog.Log(oplog.OpPlacementDecided,
							oplog.WithHost(host),
							oplog.WithDetail(placement.FormatPlacementDetail(pick.OnPrem)))
					}
				}
			} else {
				sources := []placement.CandidateSource{&placement.OnPremSource{}, &campaign.ReuseSource{}}
				if runIfOnline {
					sources = []placement.CandidateSource{&placement.OnPremSource{}}
				}
				endPlacement := rec.Phase("placement", "evaluating placement")
				plan, err := placement.Evaluate(placement.EvaluateRequest{
					Constraints: placementConstraints,
					Predictor:   predict,
					Sources:     sources,
					Database:    database,
				})
				endPlacement()
				if err != nil {
					if runIfOnline && runJSON {
						_ = emitRunReceipt(cmd, runSubmissionReceipt{
							PlacementDecision: "not_accepted",
							SourcePin:         sourcePinFromMetadata(sourceMeta),
							IdempotencyKey:    runIdempotencyKey,
						})
					}
					return err
				}
				placementPlan = plan
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
				}
			}
		}

		params := buildRunQueueParams(host)

		if err := validatePinnedHostQueueGate(host, placementConstraints); err != nil {
			return err
		}
		if runIfOnline {
			if host == "" {
				if runJSON {
					_ = emitRunReceipt(cmd, runSubmissionReceipt{
						PlacementDecision: "not_accepted",
						SourcePin:         sourcePinFromMetadata(sourceMeta),
						IdempotencyKey:    runIdempotencyKey,
					})
				}
				return fmt.Errorf("no eligible inventory host is online; no job was created")
			}
			if spec := inventory.FindHost(host); spec != nil {
				verdict := placement.CheckHostConstraintsWithActiveJobs(database, *spec, placementConstraints)
				if !verdict.Eligible {
					if runJSON {
						_ = emitRunReceipt(cmd, runSubmissionReceipt{
							PlacementDecision: "not_accepted",
							SelectedHost:      host,
							SourcePin:         sourcePinFromMetadata(sourceMeta),
							IdempotencyKey:    runIdempotencyKey,
						})
					}
					return fmt.Errorf("host %s cannot accept the job: %s; no job was created", host, strings.Join(verdict.Messages(), "; "))
				}
			}
			if !placement.ProbeHosts([]string{host}, 5*time.Second)[host] {
				if runJSON {
					_ = emitRunReceipt(cmd, runSubmissionReceipt{
						PlacementDecision: "not_accepted",
						SelectedHost:      host,
						SourcePin:         sourcePinFromMetadata(sourceMeta),
						IdempotencyKey:    runIdempotencyKey,
					})
				}
				return fmt.Errorf("host %s is offline; no job was created", host)
			}
		}

		// Concentration check: when the user's constraints look likely
		// to match a narrow slice of the offer pool, warn before the
		// job is queued so they can broaden the constraint. Stays quiet
		// on healthy pools and on cold starts. Best-effort — a query
		// failure isn't worth blocking submission for.
		if host == "" && gpuClass != "" {
			vram := 0
			if resolvedGPUMemGB != nil {
				vram = *resolvedGPUMemGB
			}
			if w, err := bidding.CheckOfferConcentration(database, gpuClass, vram); err == nil && w.Message != "" {
				fmt.Fprintf(os.Stderr, "warning: %s\n", w.Message)
			}
		}

		endSubmit := rec.Phase("submit", "submitting job")
		jobID, err := recordQueuedJobSingleWriter(database, params)
		endSubmit()
		if err != nil {
			return fmt.Errorf("submit job: %w", err)
		}
		if submissionNonce != "" {
			recorded, getErr := db.GetJobByID(database, jobID)
			if getErr != nil {
				return fmt.Errorf("read idempotent submission: %w", getErr)
			}
			if recorded.Metadata == nil || recorded.Metadata.SubmissionNonce != submissionNonce {
				if runJSON {
					return emitRunReceipt(cmd, runReceiptForJob(recorded, "deduplicated", false, true, runIdempotencyKey))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Job %s already exists for idempotency key %q\n", ids.FormatJobID(jobID), runIdempotencyKey)
				return nil
			}
		}
		recordRunPlacementTelemetry(database, cfg, jobID, "run", "fast", placementPlan, placementResult, predict)

		// Store placement telemetry if auto-placement was used
		if placementResult != nil {
			meta := buildPlacementMeta(placementResult, predict)
			if err := db.SetJobPlacementMeta(database, jobID, meta); err != nil {
				slog.Warn("failed to save placement meta", "error", err)
			}
		}

		if runIfOnline {
			if spec := inventory.FindHost(host); spec != nil {
				admittedConstraints := placementConstraints
				admittedConstraints.SelfJobID = jobID
				verdict := placement.CheckHostConstraintsWithActiveJobs(database, *spec, admittedConstraints)
				if !verdict.Eligible {
					if deleteErr := db.DeleteJob(database, jobID); deleteErr != nil {
						return fmt.Errorf("capability admission failed and cleanup of %s failed: %v", ids.FormatJobID(jobID), deleteErr)
					}
					if runJSON {
						_ = emitRunReceipt(cmd, runSubmissionReceipt{
							PlacementDecision: "not_accepted",
							SelectedHost:      host,
							SourcePin:         sourcePinFromMetadata(sourceMeta),
							IdempotencyKey:    runIdempotencyKey,
						})
					}
					return fmt.Errorf("host %s lost the admission race: %s; no job was created", host, strings.Join(verdict.Messages(), "; "))
				}
			}
			endSync := rec.Phase("sync", fmt.Sprintf("dispatching immediately to %s", host))
			syncResult, syncErr := ops.SyncHost(database, host, ops.HostSyncOptions{
				Timeout: 10 * time.Second,
				Logger:  ops.NewSilentSyncLogger(),
			}, func(h string) (bool, error) {
				return ensureQueueRunnerStarted(h)
			})
			endSync()
			job, getErr := db.GetJobByID(database, jobID)
			accepted := getErr == nil && job != nil && job.LastSyncedStatus == db.StatusQueued
			if !accepted {
				if deleteErr := db.DeleteJob(database, jobID); deleteErr != nil {
					return fmt.Errorf("immediate dispatch failed and cleanup of %s failed: %v (sync: %v)", ids.FormatJobID(jobID), deleteErr, syncErr)
				}
				if runJSON {
					_ = emitRunReceipt(cmd, runSubmissionReceipt{
						PlacementDecision: "not_accepted",
						SelectedHost:      host,
						SourcePin:         sourcePinFromMetadata(sourceMeta),
						IdempotencyKey:    runIdempotencyKey,
					})
				}
				if syncErr != nil {
					return fmt.Errorf("immediate dispatch to %s failed; no job was created: %w", host, syncErr)
				}
				return fmt.Errorf("immediate dispatch to %s was not acknowledged; no job was created (contacted=%t)", host, syncResult.HostContacted)
			}
			ensureDaemonForWork(os.Stderr)
			if runJSON {
				return emitRunReceipt(cmd, runReceiptForJob(job, "accepted_immediately", true, false, runIdempotencyKey))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Job %s accepted immediately on %s\n", ids.FormatJobID(jobID), host)
			if runWait {
				rec.PrintSummary()
				return waitForQueuedJobCompletion(database, jobID, false)
			}
			if runFollow {
				rec.PrintSummary()
				return followQueuedJob(database, jobID, host, false)
			}
			return nil
		}

		if host == "" {
			accepted, err := trySubmitJobToGraceReuse(cmd, database, jobID)
			if err != nil {
				return err
			}
			if accepted {
				if runJSON {
					job, _ := db.GetJobByID(database, jobID)
					return emitRunReceipt(cmd, runReceiptForJob(job, "accepted_immediately", true, false, runIdempotencyKey))
				}
				return nil
			}
			reasons := []string{autoPlacementPendingReason}
			if err := db.SetJobPlacementReasons(database, jobID, reasons); err != nil {
				slog.Warn("failed to save unplaced reasons", "job_id", jobID, "error", err)
			}
			ensureDaemonForWork(os.Stderr)
			if runJSON {
				job, _ := db.GetJobByID(database, jobID)
				return emitRunReceipt(cmd, runReceiptForJob(job, "queued", false, false, runIdempotencyKey))
			}
			printAutoPlacementPending(cmd.OutOrStdout(), database, jobID, autoPlacementPendingReason)
			return nil
		}

		w := cmd.OutOrStdout()
		if !runJSON {
			fmt.Fprintf(w, "Job #%d queued on %s\n", jobID, host)
		}
		if !runJSON && placementResult != nil && placementResult.CompletionEst.Mean > 0 {
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
		if !runJSON {
			printRunSubmissionExpectation(w, database, jobID)
			printSubmissionPreview(w, placementResult)
			fmt.Fprintln(w)
			fmt.Fprintf(w, "  Working dir: %s\n", workingDir)
			fmt.Fprintf(w, "  Command: %s\n", command)
			if runDescription != "" {
				fmt.Fprintf(w, "  Description: %s\n", runDescription)
			}
			if len(runEnvVars) > 0 {
				fmt.Fprintf(w, "  Env vars: %s\n", formatEnvVarsForDisplay(runEnvVars))
			}
			printDiskPreview(w, diskMeta)
			printSourceRootsPreview(w, sourceMeta, remoteSourceRootForPreview(workingDir, host))
		}

		// Push explicit-host jobs immediately. Auto-placed jobs intentionally
		// leave dispatch to the daemon so submission avoids live SSH probes.
		deferred := autoPlacedFromDB
		if autoPlacedFromDB {
			rec.Event("sync deferred — daemon will dispatch selected host")
		} else {
			deferred = !syncHostWithProgress(database, host, runNoSync, rec)
		}
		ensureDaemonForWork(os.Stderr)
		if runJSON {
			job, _ := db.GetJobByID(database, jobID)
			acceptedImmediately := job != nil && job.LastSyncedStatus == db.StatusQueued
			decision := "queued"
			if acceptedImmediately {
				decision = "accepted_immediately"
			}
			return emitRunReceipt(cmd, runReceiptForJob(job, decision, acceptedImmediately, false, runIdempotencyKey))
		}

		// wait/follow handlers call os.Exit, which would bypass the deferred
		// PrintSummary. Print it now so the user sees phase timing before
		// the wait begins. PrintSummary is idempotent.
		if runWait {
			rec.PrintSummary()
			return waitForQueuedJobCompletion(database, jobID, deferred)
		}
		if runFollow {
			rec.PrintSummary()
			return followQueuedJob(database, jobID, host, deferred)
		}
		return nil
	}

	// Below: draft or dependency modes only.

	if runDraft {
		return recordDraftRunJob(cmd, database, draftRunParams{
			Config:           cfg,
			Host:             host,
			WorkingDir:       workingDir,
			Command:          command,
			Description:      runDescription,
			ProjectName:      projectName,
			EnvVars:          runEnvVars,
			Tags:             runTags,
			GPU:              gpu,
			GPUClass:         gpuClass,
			GPUMemGB:         resolvedGPUMemGB,
			GPUMemMaxGB:      resolvedGPUMemMaxGB,
			MaxComputeCap:    persistMaxComputeCap,
			CLIOverrides:     cliOverrides,
			Inputs:           runInputs,
			BestEffortInputs: bestEffortInputs,
			Outputs:          runOutputs,
			OutputDirs:       outputDirs,
			Produces:         runProduces,
			Needs:            resolvedNeeds,
			Disk:             diskMeta,
			Source:           sourceMeta,
		})
	}

	// Placement for dependency modes (--after, --after-any).
	var placementResult *placement.PlacementResult
	if host == "" {
		endPlacement := rec.Phase("placement", "evaluating placement")
		plan, err := placement.Evaluate(placement.EvaluateRequest{
			Constraints: placementConstraints,
			Predictor:   predict,
			Sources:     []placement.CandidateSource{&placement.OnPremSource{}},
			Database:    database,
		})
		endPlacement()
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
		params := buildRunQueueParams("")
		endSubmit := rec.Phase("submit", "recording unplaced job")
		jobID, err := recordQueuedJobSingleWriter(database, params)
		endSubmit()
		if err != nil {
			return fmt.Errorf("record unplaced job: %w", err)
		}
		recordRunPlacementTelemetry(database, cfg, jobID, "run", "cheap", nil, placementResult, predict)
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

	if err := validatePinnedHostQueueGate(host, placementConstraints); err != nil {
		return err
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
		endSubmit := rec.Phase("submit", "queueing dependent job")
		res, err := queueJob(database, queueJobOptions{
			Host:             host,
			WorkingDir:       workingDir,
			Command:          command,
			Description:      runDescription,
			Project:          projectName,
			EnvVars:          runEnvVars,
			Tags:             runTags,
			GPU:              gpu,
			GPUClass:         gpuClass,
			GPUMemGB:         resolvedGPUMemGB,
			GPUMemMaxGB:      resolvedGPUMemMaxGB,
			CPUCores:         runCPUCores,
			CPUMemGB:         db.EffectiveCPUMemGB(runCPUMem, runCPUMemStrict),
			Interconnect:     runInterconnect,
			Dependencies:     deps,
			AutoStart:        true,
			Inputs:           runInputs,
			BestEffortInputs: bestEffortInputs,
			Outputs:          runOutputs,
			OutputDirs:       outputDirs,
			Produces:         runProduces,
			Needs:            resolvedNeeds,
			CloudAfter:       cloudAfter,
			GPUMemStrict:     true, // GPUMemGB is already resolved above; avoid re-applying headroom.
			Disk:             diskMeta,
			Source:           sourceMeta,
			CLIOverrides:     cliOverrides,
			MaxComputeCap:    persistMaxComputeCap,
		})
		endSubmit()
		if err != nil {
			return fmt.Errorf("queue job: %w", err)
		}
		jobID := res.JobID
		fmt.Printf("Job %s added to queue on %s, will run after job %s %s\n\n", ids.FormatJobID(jobID), host, ids.FormatJobID(afterID), waitType)
		fmt.Printf("  Working dir: %s\n", workingDir)
		fmt.Printf("  Command: %s\n", command)
		if runDescription != "" {
			fmt.Printf("  Description: %s\n", runDescription)
		}
		if len(runEnvVars) > 0 {
			fmt.Printf("  Env vars: %s\n", formatEnvVarsForDisplay(runEnvVars))
		}
		printDiskPreview(os.Stdout, diskMeta)
		printSourceRootsPreview(os.Stdout, sourceMeta, remoteSourceRootForPreview(workingDir, host))
		fmt.Printf("  After job: %d (%s)\n", afterID, waitType)

		syncHostWithProgress(database, host, runNoSync, rec)
		return nil
	}

	return nil
}

func fastSubmitPendingReasonForRecentOnPrem(current string, tags []string, recent bool, plan *placement.PlacementPlan) string {
	if !db.HasInventoryTag(tags) {
		return current
	}
	if !recent {
		return "recent host state unavailable"
	}
	if plan != nil && plan.Unplaced {
		return "waiting for an eligible on-prem host"
	}
	return current
}

func scanRunScriptMeta(localDir, command string) (*dataloc.ScriptMeta, error) {
	scriptMeta, err := dataloc.ScanScriptMeta(localDir, command)
	if err != nil {
		return nil, fmt.Errorf("invalid script metadata: %w", err)
	}
	return scriptMeta, nil
}

// normalizeInterconnect validates the flag value and phrases the error in
// flag terms. The rule itself lives in placement, which is also where script
// metadata is validated, so the two cannot accept different sets.
func normalizeInterconnect(value string) (string, error) {
	v, err := placement.NormalizeInterconnect(value)
	if err != nil {
		return "", fmt.Errorf("--interconnect must be one of %s", strings.Join(placement.InterconnectValues, ", "))
	}
	return v, nil
}

func evaluateRecentOnPremPlacement(database *sql.DB, constraints placement.Constraints) (*placement.PlacementPlan, bool, error) {
	if db.HasRentalTag(constraints.Tags) || constraints.Provider != "" {
		return &placement.PlacementPlan{Unplaced: true}, true, nil
	}
	hostNames, err := placement.LoadHostNames()
	if err != nil {
		return nil, false, fmt.Errorf("load host inventory: %w", err)
	}
	metrics, recent, err := placement.RecentHostMetrics(database, hostNames, db.HostInfoStaleThreshold)
	if err != nil || !recent {
		return nil, recent, err
	}
	scores, err := placement.ScoreHostsWithPredictor(database, constraints, metrics, nil)
	if err != nil {
		return nil, true, err
	}
	for _, s := range scores {
		if !s.Eligible || metrics[s.Host] == nil {
			continue
		}
		result := &placement.PlacementResult{
			Host:          s.Host,
			Reasons:       s.Reasons,
			Scores:        scores,
			Metrics:       metrics,
			CompletionEst: s.CompletionEst,
		}
		candidate := placement.Candidate{
			Kind:        placement.CandidateOnPrem,
			ID:          s.Host,
			DisplayName: s.Host,
			EstTime:     s.CompletionEst,
			Survival:    1.0,
			OnPrem:      result,
		}
		return &placement.PlacementPlan{
			Candidates: []placement.Candidate{candidate},
			Cheap:      &candidate,
			Fast:       &candidate,
			Fastest:    &candidate,
		}, true, nil
	}
	return &placement.PlacementPlan{Unplaced: true}, true, nil
}

func shouldValidateRentalJobImage(host string, tags []string, draft, dryRun bool) bool {
	if draft || dryRun || db.HasInventoryTag(tags) {
		return false
	}
	host = strings.TrimSpace(host)
	return host == "" || db.IsLaunchHost(host)
}

func printAutoPlacementPending(w io.Writer, database *sql.DB, jobID int64, reason string) {
	if runNoWait {
		fmt.Fprintf(w, "Job #%d accepted; placement pending (%s)\n", jobID, reason)
		printRunSubmissionExpectation(w, database, jobID)
		return
	}
	if placed := waitForAutoPlacement(database, jobID, runAutoPlacementWaitTimeout); placed != nil {
		switch placed.TargetKind() {
		case db.JobTargetInventoryHost:
			fmt.Fprintf(w, "Job #%d accepted and queued on %s\n", jobID, placed.Host)
		case db.JobTargetRentalInstance:
			if placed.LaunchID != nil {
				fmt.Fprintf(w, "Job #%d accepted and assigned to rental instance %s\n", jobID, ids.FormatInstanceID(*placed.LaunchID))
			} else {
				fmt.Fprintf(w, "Job #%d accepted and assigned to %s\n", jobID, placed.Host)
			}
		default:
			fmt.Fprintf(w, "Job #%d accepted; placement pending (%s)\n", jobID, reason)
		}
		printRunSubmissionExpectation(w, database, jobID)
		return
	}
	fmt.Fprintf(w, "Job #%d accepted; placement pending (%s)\n", jobID, reason)
	printRunSubmissionExpectation(w, database, jobID)
}

func printRunSubmissionExpectation(w io.Writer, database *sql.DB, jobID int64) {
	if w == nil || database == nil || jobID <= 0 {
		return
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		return
	}
	for _, line := range queuedExpectationLines(database, job) {
		switch line.Label {
		case "Normal range", "Action":
			fmt.Fprintf(w, "  %s: %s\n", line.Label, line.Value)
		}
	}
}

func waitForAutoPlacement(database *sql.DB, jobID int64, timeout time.Duration) *db.Job {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := db.GetJobByID(database, jobID)
		if err == nil && job != nil && job.TargetKind() != db.JobTargetUnplaced {
			return job
		}
		time.Sleep(runAutoPlacementPollInterval)
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

func printSubmissionPreview(w io.Writer, result *placement.PlacementResult) {
	if result == nil || result.CompletionEst.Mean <= 0 {
		return
	}
	est := result.CompletionEst.Mean
	fmt.Fprintf(w, "  Wall-clock preview: ~%.0fm", est.Minutes())
	if est >= 30*time.Minute {
		fmt.Fprintf(w, " (long startup/run; ensure the job emits Progress: lines or checkpoints before watchdog/grace limits)")
	}
	fmt.Fprintln(w)
}

func printDiskPreview(w io.Writer, disk *db.JobDiskMetadata) {
	if disk == nil {
		return
	}
	if disk.DiskGB > 0 {
		fmt.Fprintf(w, "  Disk floor: %dGB\n", disk.DiskGB)
	}
	if disk.DiskMaxGB > 0 {
		fmt.Fprintf(w, "  Disk ceiling: %dGB (caps the estimate, overrides the floor)\n", disk.DiskMaxGB)
	}
	runtimeGB := disk.RuntimeDiskGB
	label := "Runtime disk"
	if runtimeGB == 0 {
		runtimeGB = disk.EstimatedRuntimeDiskGB
		label = "Runtime disk est."
	}
	if runtimeGB > 0 {
		fmt.Fprintf(w, "  %s: %dGB\n", label, runtimeGB)
	}
}

func buildJobSourceMetadata(localDir string, inputs []string, commands []string) (*db.JobSourceMetadata, error) {
	if localDir == "" {
		return nil, nil
	}
	manifest, tmpPaths, err := srcsync.BuildCloudSourceManifestForInputsAndCommands(localDir, inputs, commands)
	if err != nil {
		return nil, err
	}
	for _, tmpPath := range tmpPaths {
		_ = os.Remove(tmpPath)
	}
	return jobSourceMetadataFromManifest(manifest, false), nil
}

func pinRunSourceSnapshot(ctx context.Context, localDir string, inputs []string, commands []string) (*db.JobSourceMetadata, error) {
	if localDir == "" {
		return nil, fmt.Errorf("working directory cannot be resolved to a local source directory")
	}
	client, err := newR2ClientFromConfig()
	if err != nil {
		return nil, err
	}
	uploadCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	result, err := srcsync.UploadCloudSourceRootsToR2WithProgressForInputsAndCommands(uploadCtx, client, localDir, inputs, commands, nil)
	if err != nil {
		return nil, err
	}
	return jobSourceMetadataFromManifest(result.Manifest, true), nil
}

func jobSourceMetadataFromManifest(manifest srcsync.SourceManifest, pinned bool) *db.JobSourceMetadata {
	roots := make([]db.JobSourceRootMetadata, 0, len(manifest.Roots))
	for _, root := range manifest.Roots {
		item := db.JobSourceRootMetadata{
			LocalPath:     root.LocalPath,
			MountBasename: root.MountBasename,
			MountRel:      root.MountRel,
			Origins:       root.Origins,
			Hash:          root.Hash,
			R2Key:         root.R2Key,
			SizeBytes:     root.SizeBytes,
		}
		if root.VCS != nil {
			item.VCS = &db.JobSourceVCSMetadata{
				Type:     root.VCS.Type,
				Revision: root.VCS.Revision,
				ChangeID: root.VCS.ChangeID,
				Dirty:    root.VCS.Dirty,
			}
		}
		roots = append(roots, item)
	}
	warnings := make([]string, 0, len(manifest.Warnings))
	for _, warning := range manifest.Warnings {
		if warning.Message != "" {
			warnings = append(warnings, warning.Message)
		}
	}
	meta := &db.JobSourceMetadata{Hash: manifest.Hash, Roots: roots, Warnings: warnings}
	if pinned {
		pinRoots := make([]db.JobSourcePinRootMetadata, 0, len(manifest.Roots))
		for _, root := range manifest.Roots {
			pinRoots = append(pinRoots, db.JobSourcePinRootMetadata{
				LocalPath:     root.LocalPath,
				MountBasename: root.MountBasename,
				MountRel:      root.MountRel,
				Hash:          root.Hash,
				R2Key:         root.R2Key,
				SizeBytes:     root.SizeBytes,
				Blobs:         append([]dataplane.SourceBlob(nil), root.Blobs...),
			})
		}
		meta.Pin = &db.JobSourcePinMetadata{Hash: manifest.Hash, Roots: pinRoots}
	}
	return meta
}

func remoteSourceRootForPreview(workingDir, host string) string {
	if host == "" || db.IsLaunchHost(host) {
		local := workdir.ResolveLocal(workingDir)
		if local == "" {
			return workingDir
		}
		return "/workspace/" + filepath.Base(local)
	}
	return workingDir
}

func printSourceRootsPreview(w io.Writer, source *db.JobSourceMetadata, projectRemoteRoot string) {
	if source == nil || (len(source.Roots) <= 1 && len(source.Warnings) == 0) {
		return
	}
	fmt.Fprintln(w, "  Source roots:")
	projectParent := filepath.Dir(projectRemoteRoot)
	for i, root := range source.Roots {
		rel := root.MountRel
		remote := filepath.Join(projectParent, root.MountBasename)
		if i == 0 {
			rel = "."
			remote = projectRemoteRoot
		}
		hash := root.Hash
		if len(hash) > 8 {
			hash = hash[:8]
		}
		origin := sourceRootOriginLabel(root.Origins)
		if origin != "" {
			origin = "  (" + origin + ")"
		}
		fmt.Fprintf(w, "    %-18s -> %-32s %s%s\n", rel, remote, hash, origin)
	}
	for _, warning := range source.Warnings {
		fmt.Fprintf(w, "    Warning: %s\n", warning)
	}
}

func sourceRootOriginLabel(origins []string) string {
	labels := make([]string, 0, len(origins))
	for _, origin := range origins {
		if origin == "" || origin == srcsync.SourceRootOriginProject {
			continue
		}
		labels = append(labels, origin)
	}
	return strings.Join(labels, ", ")
}

func printJobSourceMetadata(w io.Writer, source *db.JobSourceMetadata) {
	if source == nil || len(source.Roots) == 0 {
		return
	}
	identityKind := "legacy/unknown"
	closureHash := source.Hash
	if source.Pin != nil {
		identityKind = db.SourceIdentityManifestV2
		closureHash = source.Pin.Hash
	}
	fmt.Fprintf(w, "Source closure: %s (%s)\n", shortHash(closureHash), identityKind)
	if source.Execution != nil {
		fmt.Fprintf(w, "Dispatch mode:  %s\n", source.Execution.DispatchMode)
		fmt.Fprintf(w, "Agent verification: %s", source.Execution.Verification)
		if source.Execution.VerifiedSHA256 != "" {
			fmt.Fprintf(w, " %s", shortHash(source.Execution.VerifiedSHA256))
		}
		if source.Execution.VerifiedAt > 0 {
			fmt.Fprintf(w, " at %s", time.Unix(source.Execution.VerifiedAt, 0).Format(time.RFC3339))
		}
		if source.Execution.AgentVersion != "" {
			fmt.Fprintf(w, " (agent %s)", source.Execution.AgentVersion)
		}
		fmt.Fprintln(w)
	} else if source.Pin != nil {
		fmt.Fprintln(w, "Dispatch mode:  not recorded")
		fmt.Fprintln(w, "Agent verification: not recorded (legacy agent or pending dispatch)")
	}
	fmt.Fprintf(w, "Roots:          %d\n", len(source.Roots))
	for i, root := range source.Roots {
		label := root.MountRel
		if i == 0 {
			label = "."
		}
		origin := sourceRootOriginLabel(root.Origins)
		if origin != "" {
			origin = " (" + origin + ")"
		}
		fmt.Fprintf(w, "             %s %s %s%s", label, root.LocalPath, shortHash(root.Hash), origin)
		if root.VCS != nil {
			vcsID := shortHash(root.VCS.Revision)
			if root.VCS.ChangeID != "" {
				vcsID = root.VCS.ChangeID
			}
			dirty := ""
			if root.VCS.Dirty {
				dirty = " dirty"
			}
			fmt.Fprintf(w, " %s:%s%s", root.VCS.Type, vcsID, dirty)
		}
		fmt.Fprintln(w)
	}
	for _, warning := range source.Warnings {
		fmt.Fprintf(w, "             Warning: %s\n", warning)
	}
}

func shortHash(hash string) string {
	if len(hash) <= 8 {
		return hash
	}
	return hash[:8]
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
			meta.PredictedRSSUpperKB = p.PeakRSSKBUpper
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

func recordRunPlacementTelemetry(database *sql.DB, cfg *config.Config, jobID int64, operation, objective string, plan *placement.PlacementPlan, result *placement.PlacementResult, predict placement.JobPredictor) {
	if database == nil || jobID <= 0 {
		return
	}
	attemptID, err := db.GetLatestAttemptID(database, jobID)
	if err != nil {
		slog.Warn("failed to load attempt for placement telemetry", "job_id", jobID, "error", err)
	}
	var attemptPtr *int64
	if attemptID > 0 {
		attemptPtr = &attemptID
	}

	selectedKind := ""
	selectedTarget := ""
	var selectedScore *float64
	if result != nil {
		selectedKind = "on-prem"
		selectedTarget = result.Host
		for _, score := range result.Scores {
			if score.Host == result.Host {
				v := score.Total
				selectedScore = &v
				break
			}
		}
	} else if plan != nil && plan.Fast != nil {
		selectedKind = plan.Fast.Kind.String()
		selectedTarget = plan.Fast.ID
		v := -plan.Fast.EstTime.Mean.Seconds()
		selectedScore = &v
	}

	sampleKey := fmt.Sprintf("run:%d:%d:%s", jobID, attemptID, operation)
	sampled := db.ShouldSample(sampleKey, cfg.PlacementAlternativeSampleRate())
	candidates := runPlacementCandidates(database, plan, result, cfg.PlacementAlternativeTopK(), selectedTarget)
	_, err = db.RecordPlacementDecision(database, db.PlacementDecision{
		JobID:                  &jobID,
		AttemptID:              attemptPtr,
		DecisionKind:           "acted",
		Operation:              operation,
		Objective:              objective,
		PlacementPolicyVersion: placement.PolicyVersion,
		SelectedKind:           selectedKind,
		SelectedTarget:         selectedTarget,
		SelectedScore:          selectedScore,
		SampleCandidates:       sampled,
	}, candidates)
	if err != nil {
		slog.Warn("failed to record placement telemetry", "job_id", jobID, "error", err)
	}

	if predict != nil && selectedTarget != "" {
		if p := predict(selectedTarget); p != nil {
			if err := db.RecordPredictionHistory(database, db.PredictionHistory{
				JobID:     jobID,
				AttemptID: attemptPtr,
				Target:    "duration",
				Host:      selectedTarget,
				GPUClass:  selectedKind,
				Prediction: map[string]any{
					"duration_s":       p.DurationS,
					"duration_s_lower": p.DurationSLower,
					"duration_s_upper": p.DurationSUpper,
				},
				Metadata: map[string]any{
					"ood_reasons":        p.DurationOODReasons,
					"n_calibration":      p.DurationNCalibration,
					"epistemic_factor":   p.DurationEpistemicFactor,
					"placement_policy":   placement.PolicyVersion,
					"prediction_serving": "placement.JobPredictor",
				},
			}); err != nil {
				slog.Warn("failed to record prediction history", "job_id", jobID, "error", err)
			}
		}
	}
}

func runPlacementCandidates(database *sql.DB, plan *placement.PlacementPlan, result *placement.PlacementResult, limit int, selectedTarget string) []db.PlacementCandidate {
	if limit <= 0 {
		return nil
	}
	var out []db.PlacementCandidate
	if plan != nil {
		for _, c := range plan.Candidates {
			if len(out) >= limit {
				break
			}
			score := -c.EstTime.Mean.Seconds()
			estTime := c.EstTime.Mean.Seconds()
			cost := c.EstCost
			survival := c.Survival
			out = append(out, db.PlacementCandidate{
				Rank:          len(out) + 1,
				CandidateKind: c.Kind.String(),
				Target:        c.ID,
				Score:         &score,
				EstTimeS:      &estTime,
				EstCost:       &cost,
				Survival:      &survival,
				Selected:      c.ID == selectedTarget,
				Details:       cacheSnapshotDetails(database, c.ID),
			})
		}
		return out
	}
	if result == nil {
		return nil
	}
	for _, s := range result.Scores {
		if len(out) >= limit {
			break
		}
		score := s.Total
		estTime := s.CompletionEst.Mean.Seconds()
		out = append(out, db.PlacementCandidate{
			Rank:          len(out) + 1,
			CandidateKind: "on-prem",
			Target:        s.Host,
			Score:         &score,
			EstTimeS:      &estTime,
			Selected:      s.Host == selectedTarget,
			Reasons:       s.Reasons,
			Details: map[string]any{
				"eligible":          s.Eligible,
				"queue_drain_s":     s.QueueDrainEst.Mean.Seconds(),
				"transfer_s":        s.TransferEst.Mean.Seconds(),
				"run_s":             s.RunEst.Mean.Seconds(),
				"contention_factor": s.ContentionFactor,
				"cache_snapshot":    cacheSnapshotDetails(database, s.Host),
			},
		})
	}
	return out
}

func cacheSnapshotDetails(database *sql.DB, host string) map[string]any {
	if database == nil || host == "" {
		return nil
	}
	hash, count, err := dataloc.HostAssetSnapshotHash(database, host)
	if err != nil {
		return nil
	}
	return map[string]any{
		"host":        host,
		"asset_count": count,
		"md5":         hash,
	}
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

// maybeWarnNoTorchPinForCloud prints a one-liner when a cloud-bound job
// looks torch-using but the project lockfile has no torch pin from which
// weft could derive a CUDA driver floor. Skipped when:
//   - usage hints are disabled,
//   - the user has already specified a CUDA/driver floor,
//   - --host targets an inventory host (driver floor is set there at boot),
//   - the project has a torch pin (the auto-floor will fire).
func maybeWarnNoTorchPinForCloud(cmd *cobra.Command, localDir, command, host, cudaDriverMin string, scriptMeta *dataloc.ScriptMeta, gpuRequested bool) {
	if !gpuRequested {
		// A no-GPU job is CPU-placed; the driver floor is irrelevant there.
		return
	}
	if !usageHintsEnabled() || hasExplicitCUDAFloor(localDir, cudaDriverMin, scriptMeta) || localDir == "" {
		return
	}
	if host != "" {
		// Submission targets a specific host (inventory or named). Driver
		// floor matters only for auto-placed cloud rentals.
		return
	}
	pin := dataloc.ScanTorchPin(localDir)
	if pin != nil && pin.CudaVariant != "" {
		// Auto-floor will fire; no warning needed.
		return
	}
	if dataloc.ScanScriptTorchRequirement(localDir, command) != nil {
		// PEP 723 script torch dependencies are handled independently of the
		// project lock because uv resolves inline scripts in isolated envs.
		return
	}
	if !dataloc.ProjectUsesTorch(localDir) {
		return
	}
	fmt.Fprintln(cmd.ErrOrStderr(),
		"Tip: this project looks torch-using but exposes no CUDA-pinned torch wheel in uv.lock / "+
			"pyproject.toml. Cloud rentals often carry OLDER CUDA drivers than the wheel `uv sync` "+
			"resolves on the rental — and the resolved wheel sometimes needs a NEWER driver than a "+
			"typical rental has. Add `--cuda-driver-min <version>` set to whatever your resolved wheel "+
			"needs (its cuXXX tag — frequently higher than common rental drivers) to gate placement on "+
			"driver version. See docs/guides/cloud-instance-debugging.md § \"Torch driver too old\".")
}

func rejectUnlockedTorchCloudRuntime(localDir, command, host, cudaDriverMin string, tags []string, scriptMeta *dataloc.ScriptMeta, gpuRequested bool) error {
	if !gpuRequested {
		// The gate protects GPU-rental placement: it fails fast when weft can't
		// derive a CUDA driver floor to filter offers / probe the rental driver
		// before `uv sync`. A job that requests no GPU is placed CPU-only, where
		// the driver floor is moot (CPU hosts satisfy GPU floor checks vacuously),
		// so rejecting it — e.g. an API-only PEP 723 script that merely lives in a
		// torch repo — is a false positive.
		return nil
	}
	if host != "" || localDir == "" || slices.Contains(tags, db.TagInventory) {
		return nil
	}
	if hasExplicitCUDAFloor(localDir, cudaDriverMin, scriptMeta) {
		return nil
	}
	if dataloc.ScanScriptTorchRequirement(localDir, command) != nil {
		return nil
	}
	if !dataloc.ProjectUsesTorch(localDir) {
		return nil
	}
	// Cloud placement is safe only when weft can derive a CUDA wheel variant —
	// and therefore a driver floor — from the torch pin. ScanTorchPin prefers
	// uv.lock (what actually installs) and falls back to pyproject.toml; a
	// variant gives the offer filter and pre-setup driver probe a floor to
	// enforce.
	if pin := dataloc.ScanTorchPin(localDir); pin != nil && pin.CudaVariant != "" {
		return nil
	}
	// A uv.lock with no recognizable CUDA variant (a CPU/macOS wheel, or a CUDA
	// runtime family the scanner does not recognize) yields no driver floor, so
	// uv can resolve a newer CUDA wheel on the rental than weft gated on. Fail
	// closed at submit rather than at the post-`uv sync` preflight after full
	// rental spend.
	if dataloc.HasUVLock(localDir) {
		return fmt.Errorf("torch runtime is locked but weft cannot derive a CUDA wheel variant (driver floor) from uv.lock: the lock may pin a CPU/macOS torch wheel or a CUDA family weft does not recognize, so `uv sync` can resolve a newer CUDA wheel on the rental than weft can gate placement on. Pass --cuda-driver-min <version> (the CUDA version your resolved wheel needs), pin a CUDA-specific torch wheel, or target an explicit --host")
	}
	req := dataloc.ScanPyprojectTorchRequirement(localDir)
	if req == nil || !req.Exact {
		return fmt.Errorf("torch runtime is not locked for cloud placement: %s. Weft cannot derive reliable CUDA/driver floors from an unlocked torch range before choosing a rental. Run `uv lock`, use an exact torch dependency, or target an explicit --host",
			describeTorchRequirement(req))
	}
	return fmt.Errorf("torch runtime is not locked for cloud placement: pyproject.toml pins torch %s but does not expose the CUDA wheel variant. Run `uv lock`, add a CUDA-specific [tool.uv] index, pass --cuda-driver-min, or target an explicit --host",
		req.Version)
}

func describeTorchRequirement(req *dataloc.TorchRequirement) string {
	if req == nil {
		return "project looks torch-using but pyproject.toml has no direct exact torch dependency and uv.lock is missing"
	}
	if req.Spec != "" {
		return fmt.Sprintf("pyproject.toml declares torch %s and uv.lock is missing", req.Spec)
	}
	return "pyproject.toml declares torch without an exact version and uv.lock is missing"
}

func hasExplicitCUDAFloor(localDir, cudaDriverMin string, scriptMeta *dataloc.ScriptMeta) bool {
	if strings.TrimSpace(cudaDriverMin) != "" {
		return true
	}
	minDriver, minCUDA := config.ProjectCloudRequirements(localDir)
	if strings.TrimSpace(minDriver) != "" || strings.TrimSpace(minCUDA) != "" {
		return true
	}
	return scriptMeta != nil && (strings.TrimSpace(scriptMeta.MinDriver) != "" || strings.TrimSpace(scriptMeta.MinCUDA) != "")
}

func hasEnvAssignment(envVars []string, key string) bool {
	prefix := key + "="
	for _, env := range envVars {
		if strings.HasPrefix(env, prefix) {
			return true
		}
	}
	return false
}

// printCommandRecommendations checks for common command patterns and suggests
// better alternatives using CLI flags. Returns true if any recommendations were printed.
func printCommandRecommendations(command, localDir string) bool {
	if !usageHintsEnabled() {
		return false
	}
	recommendations := commandRecommendations(command, localDir)
	if len(recommendations) == 0 {
		return false
	}
	fmt.Fprintln(os.Stderr)
	for _, rec := range recommendations {
		fmt.Fprintln(os.Stderr, rec)
	}
	return true
}

func commandRecommendations(command, localDir string) []string {
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

	lowerCommand := strings.ToLower(command)
	if strings.Contains(lowerCommand, "sglang") && needsFrameworkRuntimeTip(command, localDir, "sglang") {
		recommendations = append(recommendations,
			"Tip: SGLang jobs should use the documented SGLang runtime path: PEP 723 or .weft.toml image metadata with lmsysorg/sglang:v0.5.10.post1, or lmsysorg/sglang:dev-cu13 for FP4 KV runs. See docs/guides/workflow-guide.md#tooluv-index-settings.")
	} else if needsFrameworkRuntimeTip(command, localDir, "vllm") {
		recommendations = append(recommendations,
			"Tip: vLLM jobs should declare vllm in PEP 723 or pyproject.toml and run through uv so weft can infer PyTorch CUDA image and disk headroom. See docs/guides/workflow-guide.md#script-metadata-pep-723.")
	}

	return recommendations
}

func warnPEP723ScriptEnvironmentMismatch(w io.Writer, localDir, command string) bool {
	if localDir == "" {
		return false
	}
	script := uvRunPythonScript(command)
	if script == "" || !dataloc.ScriptHasPEP723Metadata(localDir, command, script) {
		return false
	}
	fmt.Fprintf(w, "Warning: `uv run python %s` uses the project environment and project uv.lock, not the script's PEP 723 dependency environment or adjacent script lock. Use `uv run %s` to use the script environment.\n", script, script)
	return true
}

func uvRunPythonScript(command string) string {
	tokens := strings.Fields(command)
	for i := 0; i+1 < len(tokens); i++ {
		if strings.Trim(tokens[i], `"'`) != "uv" || strings.Trim(tokens[i+1], `"'`) != "run" {
			continue
		}
		for j := i + 2; j < len(tokens); j++ {
			tok := strings.Trim(tokens[j], `"'`)
			if tok == "--with" || tok == "--with-editable" || tok == "--with-requirements" {
				j++
				continue
			}
			if strings.HasPrefix(tok, "--with=") || strings.HasPrefix(tok, "--with-editable=") || strings.HasPrefix(tok, "--with-requirements=") {
				continue
			}
			if strings.HasPrefix(tok, "-") {
				continue
			}
			if !isPythonExecTokenForCommandHint(tok) {
				break
			}
			for k := j + 1; k < len(tokens); k++ {
				next := strings.Trim(tokens[k], `"'`)
				if strings.HasPrefix(next, "-") {
					continue
				}
				if strings.HasSuffix(next, ".py") {
					return next
				}
				break
			}
			break
		}
	}
	return ""
}

func isPythonExecTokenForCommandHint(token string) bool {
	base := filepath.Base(token)
	return base == "python" || base == "python2" || base == "python3" ||
		strings.HasPrefix(base, "python2.") || strings.HasPrefix(base, "python3.")
}

func needsFrameworkRuntimeTip(command, localDir, name string) bool {
	if !strings.Contains(strings.ToLower(command), name) {
		return false
	}
	if commandDeclaresPackageViaUVWith(command, name) || pyprojectDeclaresPackage(localDir, name) {
		return !commandHasUVRun(command)
	}
	if scriptDeclaresPackage(localDir, command, name) {
		return !commandUsesPEP723ScriptRunner(localDir, command)
	}
	return true
}

func commandDeclaresPackageViaUVWith(command, name string) bool {
	for _, dep := range dataloc.ScanUVRunWith(command) {
		if dep.Name == name {
			return true
		}
	}
	return false
}

func scriptDeclaresPackage(localDir, command, name string) bool {
	for _, dep := range dataloc.ParseDepSpecs(dataloc.ScanScriptDependencies(localDir, command)) {
		if dep.Name == name {
			return true
		}
	}
	return false
}

func pyprojectDeclaresPackage(localDir, name string) bool {
	if localDir == "" {
		return false
	}
	tree, err := toml.LoadFile(filepath.Join(localDir, "pyproject.toml"))
	if err != nil {
		return false
	}
	deps, ok := tree.Get("project.dependencies").([]interface{})
	if !ok {
		return false
	}
	for _, dep := range deps {
		s, ok := dep.(string)
		if !ok {
			continue
		}
		for _, spec := range dataloc.ParseDepSpecs([]string{s}) {
			if spec.Name == name {
				return true
			}
		}
	}
	return false
}

func commandHasUVRun(command string) bool {
	tokens := strings.Fields(command)
	for i := 0; i+1 < len(tokens); i++ {
		if strings.Trim(tokens[i], `"'`) == "uv" && strings.Trim(tokens[i+1], `"'`) == "run" {
			return true
		}
	}
	return false
}

func commandUsesPEP723ScriptRunner(localDir, command string) bool {
	if commandRunsScriptViaUV(command) {
		return true
	}
	return bareExecScriptHasUVScriptShebang(localDir, command)
}

func commandRunsScriptViaUV(command string) bool {
	tokens := strings.Fields(command)
	for i := 0; i+1 < len(tokens); i++ {
		if strings.Trim(tokens[i], `"'`) != "uv" || strings.Trim(tokens[i+1], `"'`) != "run" {
			continue
		}
		for j := i + 2; j < len(tokens); j++ {
			tok := strings.Trim(tokens[j], `"'`)
			if tok == "--with" || tok == "--with-editable" || tok == "--with-requirements" {
				j++
				continue
			}
			if strings.HasPrefix(tok, "--with=") || strings.HasPrefix(tok, "--with-editable=") || strings.HasPrefix(tok, "--with-requirements=") {
				continue
			}
			if strings.HasPrefix(tok, "-") {
				continue
			}
			return strings.HasSuffix(tok, ".py")
		}
	}
	return false
}

func bareExecScriptHasUVScriptShebang(localDir, command string) bool {
	if localDir == "" {
		return false
	}
	token := firstExecTokenForHint(command)
	if token == "" || !strings.HasSuffix(token, ".py") {
		return false
	}
	abs := token
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(localDir, token)
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return false
	}
	firstLine, _, _ := strings.Cut(string(content), "\n")
	return strings.HasPrefix(firstLine, "#!") &&
		strings.Contains(firstLine, "uv") &&
		strings.Contains(firstLine, "run") &&
		strings.Contains(firstLine, "--script")
}

func firstExecTokenForHint(command string) string {
	for _, tok := range strings.Fields(command) {
		tok = strings.Trim(tok, `"'`)
		if strings.Contains(tok, "=") {
			name, _, ok := strings.Cut(tok, "=")
			if ok && isShellEnvName(name) {
				continue
			}
		}
		return tok
	}
	return ""
}

func isShellEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i, c := range name {
		if i == 0 {
			if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_') {
				return false
			}
			continue
		}
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return false
		}
	}
	return true
}

// dropModelRefsShadowedByDatasetRefs removes any "hf:<id>" entry from inputs
// when "hf-dataset:<id>" is also present, returning the filtered slice and
// the dropped refs.
// hfOfflineEnvVars returns per-library offline environment for declared HF
// model inputs. Avoid HF_HUB_OFFLINE: it also forces datasets offline, and
// hf-dataset staging is not yet guaranteed to be load_dataset-offline-ready.
func hfOfflineEnvVars(inputs []string) []string {
	for _, input := range inputs {
		if strings.HasPrefix(input, "hf:") {
			return []string{"TRANSFORMERS_OFFLINE=1"}
		}
	}
	return nil
}

func dropModelRefsShadowedByDatasetRefs(inputs []string) (filtered, removed []string) {
	datasets := make(map[string]bool)
	for _, s := range inputs {
		if strings.HasPrefix(s, "hf-dataset:") {
			datasets[strings.TrimPrefix(s, "hf-dataset:")] = true
		}
	}
	if len(datasets) == 0 {
		return inputs, nil
	}
	filtered = make([]string, 0, len(inputs))
	for _, s := range inputs {
		if strings.HasPrefix(s, "hf:") && datasets[strings.TrimPrefix(s, "hf:")] {
			removed = append(removed, s)
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered, removed
}

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

func scriptMetaInputs(meta *dataloc.ScriptMeta) []string {
	if meta == nil {
		return nil
	}
	return meta.Inputs
}

func autoDetectedInputsForCommand(localDir, command string, explicitInputs []string) []string {
	detected := mergeDedup(
		dataloc.ScanPythonHFRefsForCommand(localDir, command),
		dataloc.ScanCommandHFRefs(command),
	)
	return dataloc.FilterAutoDetectedInputs(detected, explicitInputs)
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

func bestEffortAutoDetectedInputs(finalInputs, autoDetectedInputs, explicitInputs []string) []string {
	if len(finalInputs) == 0 || len(autoDetectedInputs) == 0 {
		return nil
	}
	autoDetected := make(map[string]bool, len(autoDetectedInputs))
	for _, input := range autoDetectedInputs {
		autoDetected[input] = true
	}
	explicit := make(map[string]bool, len(explicitInputs))
	for _, input := range explicitInputs {
		explicit[input] = true
	}
	bestEffort := make([]string, 0, len(autoDetectedInputs))
	for _, input := range finalInputs {
		if autoDetected[input] && !explicit[input] {
			bestEffort = append(bestEffort, input)
		}
	}
	return bestEffort
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

func persistDraftArtifactFields(database *sql.DB, jobID int64, inputs, outputs, outputDirs, produces, needs []string) error {
	if len(inputs) > 0 {
		if err := db.SetJobInputs(database, jobID, inputs); err != nil {
			return fmt.Errorf("set draft job inputs: %w", err)
		}
	}
	if len(outputs) > 0 {
		if err := db.SetJobOutputs(database, jobID, outputs); err != nil {
			return fmt.Errorf("set draft job outputs: %w", err)
		}
	}
	if len(outputDirs) > 0 {
		if err := db.SetJobOutputDirs(database, jobID, outputDirs); err != nil {
			return fmt.Errorf("set draft job output dirs: %w", err)
		}
	}
	if len(produces) > 0 {
		if err := db.SetJobProduces(database, jobID, produces); err != nil {
			return fmt.Errorf("set draft job produces: %w", err)
		}
	}
	if len(needs) > 0 {
		if err := db.SetJobNeeds(database, jobID, needs); err != nil {
			return fmt.Errorf("set draft job needs: %w", err)
		}
		job, err := db.GetJobByID(database, jobID)
		if err != nil {
			return fmt.Errorf("verify draft job needs persistence: %w", err)
		}
		if !equalStringSlices(job.Needs, needs) {
			return fmt.Errorf("verify draft job needs persistence: stored=%v requested=%v", job.Needs, needs)
		}
	}
	return nil
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
		return ensureQueueRunnerStarted(h)
	})
	return syncResult.HostContacted
}

// syncHostWithProgress runs the post-submission sync and emits phase-recorder
// progress lines for each outcome. Returns true if the host was contacted
// and the job was pushed; false when the sync was skipped (--no-sync), the
// host was empty, or the host was unreachable.
func syncHostWithProgress(database *sql.DB, host string, noSync bool, rec *runPhaseRecorder) bool {
	if noSync {
		rec.Event("sync skipped (--no-sync) — will sync on next background sync")
		return false
	}
	if host == "" {
		return false
	}
	endSync := rec.Phase("sync", fmt.Sprintf("syncing host %s", host))
	ok := syncHostQuietly(database, host, false)
	endSync()
	if !ok {
		rec.Event(fmt.Sprintf("sync deferred — %s offline, will retry on next sync", host))
	}
	return ok
}
