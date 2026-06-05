package runner

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/opsqueue"
	srcsync "github.com/osteele/weft/internal/sync"
)

// Runner is the main queue runner that manages job execution.
type Runner struct {
	queueDir     string
	logDir       string
	setupTimeout time.Duration

	state           *State
	cmdProc         *CommandProcessor
	gpuInv          *GPUInventory
	cpuConfig       CPUConfig
	telemetryConfig TelemetryConfig
	benchCfg        BenchmarkConfig
	cpuCount        int

	// File paths
	commandsFile string
	stateFile    string
	currentFile  string
	pidFile      string
	runnerLog    string

	// Runtime state
	processes      map[string]*Process // jobID -> process
	hookStopFuncs  map[string]func()   // jobID -> stop function from OnJobStart
	processesMu    sync.Mutex
	lastSampleTime time.Time

	// Benchmark tracking
	benchmarkIdleCount  int
	benchmarkLastReason string

	// Lifecycle hooks (optional, best-effort)
	OnJobStart  func(jobID int64, logPath string) func() // returns stop function for live upload
	OnJobFinish func(jobID int64, logDir string, exitCode int)

	// EnsureSourceFromR2 is an optional preflight hook used by jobs queued
	// in R2-isolated mode (CommandJob.SourceR2Key != ""). The agent
	// implementation downloads sources/<sha>.tar.gz from R2 (rclone) and
	// extracts it into perJobDir, then returns nil. A non-nil error causes
	// the runner to reject the attempt at preflight (no startTime, no meta
	// file, sentinel + failure_reason written). When nil, jobs with
	// SourceR2Key fall back to the shared working dir with a debug log.
	EnsureSourceFromR2 func(jobID int64, r2Key, perJobDir string) error

	// Shutdown
	stopCh chan struct{}
}

// Config holds configuration for the runner.
type Config struct {
	QueueDir     string
	LogDir       string
	SetupTimeout time.Duration // If >0, kill setup commands after this duration
}

// DefaultConfig returns a configuration with standard paths.
func DefaultConfig() Config {
	home, _ := os.UserHomeDir()
	return Config{
		QueueDir: filepath.Join(home, ".cache", "weft", "queue"),
		LogDir:   filepath.Join(home, ".cache", "weft", "logs"),
	}
}

// New creates a new Runner with the given configuration.
func New(cfg Config) *Runner {
	return &Runner{
		queueDir:        cfg.QueueDir,
		logDir:          cfg.LogDir,
		setupTimeout:    cfg.SetupTimeout,
		commandsFile:    filepath.Join(cfg.QueueDir, opsqueue.CommandsFileName()),
		stateFile:       filepath.Join(cfg.QueueDir, opsqueue.StateFileName()),
		currentFile:     filepath.Join(cfg.QueueDir, opsqueue.CurrentFileName()),
		pidFile:         filepath.Join(cfg.QueueDir, opsqueue.PidFileName()),
		runnerLog:       filepath.Join(cfg.QueueDir, opsqueue.RunnerLogName()),
		cpuConfig:       DefaultCPUConfig(),
		telemetryConfig: DefaultTelemetryConfig(),
		benchCfg:        DefaultBenchmarkConfig(),
		processes:       make(map[string]*Process),
		hookStopFuncs:   make(map[string]func()),
		stopCh:          make(chan struct{}),
	}
}

// Run starts the main loop. Blocks until shutdown.
func (r *Runner) Run() error {
	// Create directories
	os.MkdirAll(r.queueDir, 0755)
	os.MkdirAll(r.logDir, 0755)

	// Initialize ops logging to runner log
	if err := oplog.Init(r.runnerLog, 0); err != nil {
		fmt.Fprintf(os.Stderr, "warning: init runner log: %v\n", err)
	}
	defer oplog.Close()

	// Discover hardware
	r.gpuInv = DiscoverGPUs()
	r.cpuCount = DetectCPUCount()
	r.cmdProc = NewCommandProcessor(r.commandsFile, r.queueDir, r.logDir)

	// Load state
	var err error
	r.state, err = LoadState(r.stateFile)
	if err != nil {
		return fmt.Errorf("load state: %w", err)
	}

	// Write PID file
	os.WriteFile(r.pidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644)

	// Setup signal handling
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		oplog.Log(oplog.OpQueueStop, oplog.WithDetailf("signal: %s", sig))
		// Write kill reason for all running jobs before shutdown
		r.processesMu.Lock()
		ids := r.state.RunningIDs()
		r.processesMu.Unlock()
		for _, jobIDStr := range ids {
			jobID := mustParseInt64(jobIDStr)
			paths := NewJobPaths(r.logDir, jobID)
			WriteKillReasonFile(paths, KillReasonRunnerShutdown)
		}
		close(r.stopCh)
	}()

	// Cleanup on exit
	defer func() {
		os.Remove(r.pidFile)
		os.Remove(r.currentFile)
	}()

	oplog.Log(oplog.OpQueueStart, oplog.WithDetailf("pid=%d", os.Getpid()))
	fmt.Printf("Queue runner started\n")
	fmt.Printf("Commands file: %s\n", r.commandsFile)
	fmt.Printf("State file: %s\n", r.stateFile)
	fmt.Printf("PID: %d\n\n", os.Getpid())

	return r.mainLoop()
}

func (r *Runner) mainLoop() error {
	mainTicker := time.NewTicker(5 * time.Second)
	defer mainTicker.Stop()

	sampleTicker := time.NewTicker(time.Duration(r.cpuConfig.SampleInterval) * time.Second)
	defer sampleTicker.Stop()
	var telemetryTicker *time.Ticker
	if r.telemetryConfig.Enabled && r.telemetryConfig.Interval > 0 {
		telemetryTicker = time.NewTicker(r.telemetryConfig.Interval)
		defer telemetryTicker.Stop()
	}

	// Run once immediately before entering the ticker loop
	if err := r.tick(); err != nil {
		return err
	}

	for {
		select {
		case <-r.stopCh:
			oplog.Log(oplog.OpQueueStop, oplog.WithDetail("signal received"))
			return nil

		case <-mainTicker.C:
			if err := r.tick(); err != nil {
				return err
			}

		case <-sampleTicker.C:
			r.adjustRunningJobAllotments()
		case <-tickerChan(telemetryTicker):
			r.sampleRunningJobs()
		}
	}
}

func (r *Runner) tick() error {
	// Process new commands
	result, err := r.cmdProc.ProcessCommands(r.state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "process commands: %v\n", err)
	}

	if result.RestartRequested {
		r.saveState()
		// Re-exec ourselves
		oplog.Log("cmd.restart")
		exe, _ := os.Executable()
		syscall.Exec(exe, os.Args, mergeEnvVars(os.Environ(), result.RestartEnv))
	}

	// Refresh running jobs (check for completion)
	r.refreshRunningJobs()

	// Save state after processing
	r.saveState()

	// Check stop condition
	if r.state.IsStopRequested() && r.state.PendingEmpty() && r.state.RunningCount() == 0 {
		oplog.Log(oplog.OpQueueStop, oplog.WithDetail("stop command received"))
		fmt.Println("Stop command received, exiting...")
		close(r.stopCh)
		return nil
	}

	// Try to start next job
	r.tryStartNextJob()

	// Write current file
	r.updateCurrentFile()

	return nil
}

func (r *Runner) tryStartNextJob() {
	if r.state.PendingEmpty() {
		return
	}

	// Don't start during warmup
	if r.warmupActive() {
		r.annotateBenchmarkWarmupWait()
		return
	}

	if r.state.IsStopRequested() {
		return
	}

	// Pop the next job
	jobID, ok := r.state.PopPending()
	if !ok {
		return
	}

	// Load job data
	job, err := ReadJobFile(r.queueDir, jobID)
	if err != nil {
		reason := fmt.Sprintf("missing queue payload: %v", err)
		fmt.Fprintf(os.Stderr, "Job %s: cannot read job file: %v\n", ids.FormatJobID(jobID), err)
		r.state.AddPendingWithReason(jobID, reason)
		r.saveState()
		return
	}
	rj := &RunnerJob{Data: job, ID: jobID}

	// Check exclusive constraints
	if AnyRunningExclusive(r.state, r.queueDir) {
		r.state.AddPendingWithReason(jobID, "exclusive gate: waiting for another exclusive job to finish")
		r.saveState()
		return
	}

	runningCount := r.state.RunningCount()
	if HasExclusiveOrBenchmarkTag(rj) && runningCount > 0 {
		reason := fmt.Sprintf("exclusive gate: waiting for %d running job(s) to finish", runningCount)
		if HasBenchmarkTag(rj) {
			reason = fmt.Sprintf("benchmark gate: waiting for %d running job(s) to finish", runningCount)
		}
		r.state.AddPendingWithReason(jobID, reason)
		r.saveState()
		return
	}

	// Benchmark: wait for system idle
	if HasBenchmarkTag(rj) {
		reason := r.benchCfg.SystemIdleCheck()
		if reason != "" {
			if reason != r.benchmarkLastReason {
				oplog.LogJob("benchmark.waiting", jobID, "", oplog.WithDetail(reason))
				r.benchmarkLastReason = reason
			}
			r.benchmarkIdleCount = 0
			r.state.AddPendingWithReason(jobID, "benchmark gate: "+reason)
			r.saveState()
			return
		}
		r.benchmarkIdleCount++
		if r.benchmarkIdleCount < r.benchCfg.IdleSamples {
			r.state.AddPendingWithReason(jobID, fmt.Sprintf("benchmark gate: confirming idle (%d/%d)", r.benchmarkIdleCount, r.benchCfg.IdleSamples))
			r.saveState()
			return
		}
		oplog.LogJob("benchmark.idle_confirmed", jobID, "", oplog.WithDetailf("samples=%d", r.benchmarkIdleCount))
		r.benchmarkIdleCount = 0
		r.benchmarkLastReason = ""
	}

	// Check CPU capacity
	if runningCount > 0 {
		currentAllotment := r.state.TotalAllotment()
		nextAllotment := r.jobAllotment(job)
		if currentAllotment+nextAllotment > r.cpuConfig.HostUtilizationTarget {
			r.state.AddPendingWithReason(jobID, fmt.Sprintf("cpu gate: %d%% + %d%% > %d%% target", currentAllotment, nextAllotment, r.cpuConfig.HostUtilizationTarget))
			r.saveState()
			return
		}
	}

	// Host-wide measured load gate. Independent of weft's allotment
	// bookkeeping; catches overload from non-weft users on shared hosts,
	// BLAS/OMP overdraft beyond declared cores, and any other source of
	// load the per-job CPU allotment system has no visibility into.
	if r.cpuConfig.HostLoadCeiling > 0 && r.cpuCount > 0 {
		loadPct := int((HostLoadAvg1() * 100.0) / float64(r.cpuCount))
		if loadPct >= r.cpuConfig.HostLoadCeiling {
			r.state.AddPendingWithReason(jobID, fmt.Sprintf("host load gate: %d%% >= %d%% ceiling (1-min loadavg / %d cores)", loadPct, r.cpuConfig.HostLoadCeiling, r.cpuCount))
			r.saveState()
			return
		}
	}

	// Refresh actual GPU memory snapshot before GPU checks (only for GPU jobs)
	jobHasGPU := job.GPUClass != "" || len(GetJobGPUDevices(job)) > 0
	if jobHasGPU {
		r.gpuInv.RefreshDeviceMemSnapshot()
	}

	// Check GPU capacity and resolve devices
	var resolvedGPUDevices []string
	if jobHasGPU {
		var canStart bool
		var gpuReason string
		canStart, resolvedGPUDevices, gpuReason = r.gpuInv.CanStartGPUJobWithReason(r.state, rj)
		if !canStart {
			if gpuReason == "" {
				gpuReason = "gpu gate: waiting for requested GPU capacity"
			} else {
				gpuReason = "gpu gate: " + gpuReason
			}
			r.state.AddPendingWithReason(jobID, gpuReason)
			r.saveState()
			return
		}
	}

	// Auto-assign GPU on hosts with GPUs when the job has no GPU constraints.
	// Without this, CUDA defaults to GPU 0, causing OOM when GPU 0 is loaded.
	if !jobHasGPU && len(r.gpuInv.Devices) > 0 {
		r.gpuInv.RefreshDeviceMemSnapshot()
		if device := r.gpuInv.PickLeastLoadedGPU(r.state); device != "" {
			resolvedGPUDevices = []string{device}
		}
	}

	// Start the job
	slog.Info("job starting", "component", "runner", "job_id", jobID, "running_count", runningCount, "gpu_class", job.GPUClass, "resolved_gpu", resolvedGPUDevices)
	err = r.startJob(jobID, job, resolvedGPUDevices)
	if err == errRequeue {
		slog.Debug("job requeued", "component", "runner", "job_id", jobID)
		r.state.AddPending(jobID)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "Job %s: start failed: %v\n", ids.FormatJobID(jobID), err)
	} else {
		slog.Info("job started successfully", "component", "runner", "job_id", jobID)
	}
	r.saveState()
}

var errRequeue = fmt.Errorf("requeue")

func (r *Runner) annotateBenchmarkWarmupWait() {
	jobID, ok := r.state.PeekPending()
	if !ok {
		return
	}
	job, err := ReadJobFile(r.queueDir, jobID)
	if err != nil || !HasBenchmarkTag(&RunnerJob{Data: job, ID: jobID}) {
		return
	}
	r.state.SetPendingReason(jobID, "benchmark gate: waiting for propitious conditions (runner warmup)")
	r.saveState()
}

func (r *Runner) startJob(jobID int64, job *opsqueue.CommandJob, preResolvedGPUDevices []string) error {
	jobIDStr := strconv.FormatInt(jobID, 10)
	command := job.Cmd
	if command == "" {
		fmt.Printf("Job %s: no command found, skipping\n", ids.FormatJobID(jobID))
		return nil
	}

	// Check if already completed or running
	if JobCompleted(r.logDir, jobID) {
		fmt.Printf("Job %s: already completed, skipping\n", ids.FormatJobID(jobID))
		removeJobFile(r.queueDir, jobID)
		return nil
	}

	// Check dependencies
	depResult := CheckDependencies(job.Deps, job.Needs, r.logDir)
	switch depResult.Result {
	case DepWaiting:
		fmt.Printf("Job %s: waiting for dependencies\n", ids.FormatJobID(jobID))
		return errRequeue
	case DepFailed:
		oplog.LogJob("job.skipped", jobID, "", oplog.WithDetailf("dependency %s failed", depResult.FailedDep))
		fmt.Printf("Job %s: skipped, dependency %s failed\n", ids.FormatJobID(jobID), depResult.FailedDep)
		paths := NewJobPaths(r.logDir, jobID)
		os.WriteFile(paths.Log, []byte(fmt.Sprintf("SKIPPED: dependency %s failed\n", depResult.FailedDep)), 0644)
		os.WriteFile(paths.Status, []byte("1\n"), 0644)
		removeJobFile(r.queueDir, jobID)
		return nil
	}

	// Resolve GPU devices (use pre-resolved if available, otherwise resolve now)
	rj := &RunnerJob{Data: job, ID: jobID}
	var gpuDevices []string
	jobHasGPU := job.GPUClass != "" || len(GetJobGPUDevices(job)) > 0

	if len(preResolvedGPUDevices) > 0 {
		gpuDevices = preResolvedGPUDevices
	} else if jobHasGPU {
		canStart, resolvedGPUDevices := r.gpuInv.CanStartGPUJob(r.state, rj)
		if !canStart {
			return errRequeue
		}
		gpuDevices = resolvedGPUDevices
	}

	paths := NewJobPaths(r.logDir, jobID)

	// Expand ~ in working directory (needed for the preflight marker read).
	expandedDir := job.Dir
	if len(expandedDir) > 1 && expandedDir[0] == '~' {
		home, _ := os.UserHomeDir()
		expandedDir = home + expandedDir[1:]
	}

	// Archive previous attempt's artifacts before either preflight or full
	// startup runs, so a fresh attempt starts with a clean slate. A rename
	// failure here would leave the prior .status in place and cause
	// JobCompleted() to skip the job on the next tick — log loudly.
	if err := ArchiveExistingFiles(r.logDir, jobID); err != nil {
		slog.Warn("archive prior attempt artifacts", "component", "runner", "job_id", jobID, "error", err)
	}

	// Preflight (Layer D): R2-isolated source. When the dispatcher
	// escalated this job to R2-isolated mode after a prior provenance
	// failure, download + extract the original tarball into a per-job dir
	// and run from there. Bypasses the marker comparison since the SHA is
	// implicit in the content-addressed R2 key.
	if job.SourceR2Key != "" {
		if r.EnsureSourceFromR2 == nil {
			return r.rejectPreflight(jobID, paths, fmt.Sprintf("r2_isolated_source_unavailable: SourceR2Key=%s but the runner has no EnsureSourceFromR2 hook", job.SourceR2Key))
		}
		perJobDir := perJobSourceDir(jobID)
		if err := r.EnsureSourceFromR2(jobID, job.SourceR2Key, perJobDir); err != nil {
			return r.rejectPreflight(jobID, paths, fmt.Sprintf("r2_isolated_source_fetch_failed: %v", err))
		}
		expandedDir = perJobDir
	} else if job.SourceSHA != "" {
		// Preflight (Layer A+C): per-job source provenance check. Runs
		// before startTime is stamped so a rejection isn't recorded as a
		// 0-second "completed" attempt. On failure: write the
		// failure_reason file (read by the agent's batch-status) and the
		// preflight_rejected sentinel (signals to the coordinator that
		// this attempt never started), then return.
		markerSHA, err := srcsync.ReadSourceMarkerForJob(expandedDir, jobID)
		if err != nil {
			msg := fmt.Sprintf("source_provenance_mismatch: expected=%s, marker unreadable in %s (%v)", job.SourceSHA, expandedDir, err)
			return r.rejectPreflight(jobID, paths, msg)
		}
		if markerSHA != job.SourceSHA {
			msg := fmt.Sprintf("source_provenance_mismatch: expected=%s, marker=%s", job.SourceSHA, markerSHA)
			return r.rejectPreflight(jobID, paths, msg)
		}
	}

	startTime := time.Now().Unix()

	oplog.LogJob(oplog.OpJobStart, jobID, "", oplog.WithDetailf("cmd=%s", command))
	fmt.Printf("==========================================\n")
	fmt.Printf("Starting job %s\n", ids.FormatJobID(jobID))
	fmt.Printf("  Working dir: %s\n", job.Dir)
	fmt.Printf("  Command: %s\n", command)
	if job.Desc != "" {
		fmt.Printf("  Description: %s\n", job.Desc)
	}
	fmt.Printf("  Log: %s\n", paths.Log)
	fmt.Printf("==========================================\n")

	// Write metadata
	WriteMetaFile(paths, jobID, job.Dir, command, job.Desc, startTime, job.SourceSHA)

	// Write log header
	WriteLogHeader(paths, jobID, job.Dir, command, job.SourceSHA)

	// Call OnJobStart hook (best-effort)
	if r.OnJobStart != nil {
		if stopFn := r.OnJobStart(jobID, paths.Log); stopFn != nil {
			r.processesMu.Lock()
			r.hookStopFuncs[jobIDStr] = stopFn
			r.processesMu.Unlock()
		}
	}

	// Write gpu_devices to meta file so the coordinator can discover them
	if len(gpuDevices) > 0 {
		if f, err := os.OpenFile(paths.Meta, os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			fmt.Fprintf(f, "gpu_devices=%s\n", strings.Join(gpuDevices, ","))
			f.Close()
		}
	}

	// Build environment
	var envVars []string

	// Load dotenv files from working directory
	if expandedDir != "" {
		dotenvVars, _ := LoadDotenvFiles(expandedDir)
		envVars = append(envVars, dotenvVars...)
	}

	setupCmd := DetectSetupCommand(expandedDir)
	if setupCmd == "uv sync" {
		scriptMeta, _ := dataloc.ScanScriptMeta(expandedDir, command)
		if ShouldSkipSetup(setupCmd, scriptMeta) {
			slog.Info("skipping uv sync: script metadata declares isolated = true",
				"component", "runner", "job_id", jobID)
			setupCmd = ""
		}
	}
	if setupCmd == direnvSetupCommand {
		resolvedEnv, ei, err := ResolveDirenvEnv(expandedDir, envVars, paths.Log)
		if err != nil {
			WriteStatusFile(paths, ei)
			return fmt.Errorf("prepare .envrc environment: %w", err)
		}
		envVars = resolvedEnv
		setupCmd = ""
	}
	if setupCmd == "" {
		WarnIfWorkdirMissingEnv(expandedDir, jobID, paths.Log)
	}

	// Apply job env vars (override dotenv)
	envVars = append(envVars, job.Env...)
	envVars = artifacts.MergeEnvVars(envVars, jobID)

	// Inject resolved GPU device
	if len(gpuDevices) > 0 && job.GPUClass != "" {
		cudaEnv := FormatGPUDeviceEnv(gpuDevices)
		envVars = append(envVars, cudaEnv)
	}

	// Run environment setup as a separate phase. Use expandedDir, not
	// job.Dir: in R2-isolated mode the latter still points at the
	// user-facing dir while the extracted sources live under expandedDir.
	if setupCmd != "" {
		ei, setupErr := RunSetupCommand(setupCmd, jobID, expandedDir, envVars, paths, r.setupTimeout)
		if setupErr != nil {
			failureReason := DetectFailureReasonFromExitInfoAndLog(ei, paths.Log)
			WriteFailureReasonFile(paths, failureReason)
			oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetailf("setup failed exit=%d", ei.ExitCode))
			return setupErr
		}
		if setupCmd == "uv sync" {
			collectAndWriteUVManifest(jobID, expandedDir, filepath.Dir(paths.Log))
		}
	}

	envVars = applyUVRunEnvAdditions(expandedDir, setupCmd, envVars, jobID, paths.Log)

	// Wrap the command so bash writes the exit code and log footer even if
	// the Go runner crashes mid-job (e.g., during a runner restart).
	wrappedCommand := WrapCommandWithExitCapture(command, paths.Status)

	// Start the process. Use expandedDir, not job.Dir: in R2-isolated mode
	// job.Dir is the user-facing path but the runtime sources are under
	// expandedDir. In normal mode expandedDir is just job.Dir with ~/
	// expanded, so the call is equivalent.
	slog.Debug("launching process", "component", "runner", "job_id", jobID)
	proc, err := StartProcess(wrappedCommand, expandedDir, envVars, paths.Log)
	if err != nil {
		oplog.LogJob(oplog.OpJobStartFailed, jobID, "", oplog.WithError(err))
		slog.Warn("job start failed", "component", "runner", "job_id", jobID, "error", err)
		os.WriteFile(paths.Status, []byte("1\n"), 0644)
		return fmt.Errorf("start process: %w", err)
	}

	// Write PID/PGID files
	slog.Debug("started process, writing PID files", "component", "runner", "job_id", jobID, "pid", proc.PID)
	proc.WritePIDFiles(paths)

	// Track the process
	r.processesMu.Lock()
	r.processes[jobIDStr] = proc
	r.processesMu.Unlock()

	// Start wait goroutine. Pass the resolved working dir (possibly the
	// per-job R2-isolated source dir) so output discovery and per-job
	// cleanup operate on the directory where the job actually ran.
	go r.waitForJob(jobID, proc, paths, startTime, rj, expandedDir)

	// Update running state
	allotment := r.jobAllotment(job)
	gpuMemGB := GetJobGPUMem(job, DefaultGPUMemGB)
	telemetryPolicy := TelemetryPolicyForJob(job)

	r.state.AddRunning(jobIDStr, RunningJobState{
		RunID:                    job.RunID,
		StartedAt:                startTime,
		WarmupUntil:              startTime + int64(r.cpuConfig.WarmupDuration),
		LocalAllotment:           allotment,
		Samples:                  []int{},
		GPUDevices:               gpuDevices,
		GPUMemGB:                 gpuMemGB,
		DiskPath:                 expandedDir,
		TelemetryIntervalSeconds: int64(telemetryPolicy.Interval / time.Second),
		TelemetryAdvancedGPU:     telemetryPolicy.CollectAdvancedGPU,
	})
	if r.telemetryConfig.Enabled {
		rs, _ := r.state.GetRunning(jobIDStr)
		SampleJob(proc.PID, proc.PGID, r.cpuCount, paths, &rs, "multi")
		r.state.SetRunning(jobIDStr, rs)
	}

	return nil
}

// rejectPreflight records a preflight rejection without ever marking the
// attempt as started. It writes the failure_reason file and a
// preflight_rejected sentinel (no status file, no meta, no log header), emits
// oplog.OpJobStartFailed, and removes the job from the runner queue so it
// won't be re-attempted on the next sweep. The coordinator's batch-status
// reconciler picks up the sentinel and closes the attempt with NULL
// timestamps and the populated failure reason.
func (r *Runner) rejectPreflight(jobID int64, paths JobPaths, reason string) error {
	if err := WriteFailureReasonFile(paths, reason); err != nil {
		slog.Warn("preflight reject: write failure_reason failed",
			"component", "runner", "job_id", jobID, "error", err)
	}
	if err := WritePreflightRejectedFile(paths); err != nil {
		slog.Warn("preflight reject: write sentinel failed",
			"component", "runner", "job_id", jobID, "error", err)
	}
	oplog.LogJob(oplog.OpJobStartFailed, jobID, "", oplog.WithDetail(reason))
	removeJobFile(r.queueDir, jobID)
	return fmt.Errorf("preflight rejected: %s", reason)
}

func (r *Runner) waitForJob(jobID int64, proc *Process, paths JobPaths, startTime int64, rj *RunnerJob, runDir string) {
	jobIDStr := strconv.FormatInt(jobID, 10)
	err := proc.Cmd.Wait()
	ei := ExtractExitInfo(err)
	slog.Info("process exited", "component", "runner", "job_id", jobID, "exit_code", ei.ExitCode, "signaled", ei.Signaled, "error", err)

	endTime := time.Now().Unix()
	duration := endTime - startTime

	// Write status and log footer. The wrapper shell may have already
	// written these (see WrapCommandWithExitCapture), so skip if present.
	if _, statErr := os.Stat(paths.Status); statErr != nil {
		WriteStatusFile(paths, ei)
		WriteLogFooter(paths, ei)
	}

	// Write artifact satisfied files for producer jobs
	for _, spec := range rj.Data.Produces {
		parsed := ParseProducesSpec(spec)
		version := parsed.Version
		if version == 0 {
			version = jobID
		}
		satisfiedPath := ArtifactSatisfiedFile(r.logDir, parsed.Path, version)
		if err := os.WriteFile(satisfiedPath, []byte(fmt.Sprintf("%d\n", ei.ExitCode)), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Job %s: failed to write artifact satisfied file %s: %v\n", ids.FormatJobID(jobID), satisfiedPath, err)
		}
	}
	if ei.ExitCode == 0 {
		if err := RecordProducedArtifacts(jobID, rj.Data.Produces); err != nil {
			fmt.Fprintf(os.Stderr, "Job %s: failed to write artifact manifest from --produces: %v\n", ids.FormatJobID(jobID), err)
			WriteManifestErrorFile(paths, "post-exit: "+err.Error())
		}
	}

	// On failure, detect the failure reason using signal-aware detection
	var failureReason string
	if ei.ExitCode != 0 {
		failureReason = DetectFailureReasonFromExitInfoAndLog(ei, paths.Log)
		WriteFailureReasonFile(paths, failureReason)
	}

	// Append end_time to meta file
	if f, err := os.OpenFile(paths.Meta, os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		fmt.Fprintf(f, "end_time=%d\n", endTime)
		f.Close()
	}

	// Log completion and discover outputs
	var outputFiles []OutputFile
	if ei.ExitCode == 0 {
		dirs := rj.Data.OutputDirs
		if len(dirs) == 0 {
			dirs = config.DefaultOutputDirs
		}
		if discovered, err := DiscoverOutputs(runDir, dirs); err == nil && len(discovered) > 0 {
			outputFiles = discovered
			oplog.LogJob("job.outputs_discovered", jobID, "", oplog.WithDetailf("files=%d total_mb=%d", len(discovered), TotalSizeMB(discovered)))
			fmt.Printf("Job %s: discovered %d output files\n", ids.FormatJobID(jobID), len(discovered))
		}
		oplog.LogJob(oplog.OpJobComplete, jobID, "", oplog.WithDetailf("exit=0 duration=%ds", duration))
		fmt.Printf("Job %s completed successfully\n", ids.FormatJobID(jobID))
	} else {
		reason := ReadFailureReasonFile(paths.FailureReason)
		detail := fmt.Sprintf("exit=%d duration=%ds reason=%s", ei.ExitCode, duration, reason)
		if ei.Signaled {
			detail += fmt.Sprintf(" signal=%s", ei.SignalName())
		}
		oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetail(detail))
		fmt.Printf("Job %s failed with exit code %d (%s)\n", ids.FormatJobID(jobID), ei.ExitCode, reason)
	}

	// Write rusage and completion record
	r.processesMu.Lock()
	rs, _ := r.state.GetRunning(jobIDStr)
	stopFn := r.hookStopFuncs[jobIDStr]
	delete(r.hookStopFuncs, jobIDStr)
	r.processesMu.Unlock()
	WriteRusageFile(paths, rs)
	killReason := ReadKillReasonFile(paths.KillReason)
	WriteCompletionRecord(paths, ei, rs, killReason, failureReason, startTime, endTime, outputFiles)

	// Stop live log uploader, then call OnJobFinish (best-effort)
	if stopFn != nil {
		stopFn()
	}
	if r.OnJobFinish != nil {
		r.OnJobFinish(jobID, filepath.Dir(paths.Log), ei.ExitCode)
	}

	// Record finished and remove from running
	r.state.RecordFinished(jobIDStr, ei.ExitCode, endTime)
	r.state.RemoveRunning(jobIDStr)

	// Cleanup
	r.processesMu.Lock()
	delete(r.processes, jobIDStr)
	r.processesMu.Unlock()

	CleanupPIDFiles(paths)
	removeJobFile(r.queueDir, jobID)
	removePerJobSourceMarker(rj.Data.Dir, jobID)
	// In R2-isolated mode the runtime source lives under a per-job dir we
	// own; remove it now so ~/.cache/weft/jobs/ doesn't grow unbounded.
	if rj.Data.SourceR2Key != "" {
		removePerJobSourceDir(jobID)
	}

	r.saveState()
}

// perJobSourceDir returns the per-job working directory used in R2-isolated
// mode (CommandJob.SourceR2Key). Tarballs are extracted here so jobs running
// off the same project don't share filesystem state.
func perJobSourceDir(jobID int64) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "weft", "jobs", fmt.Sprintf("%d", jobID), "source")
}

// removePerJobSourceDir removes ~/.cache/weft/jobs/<id>/ for R2-isolated
// jobs after they complete. Best-effort: a stale dir wastes disk but is
// otherwise harmless; the next R2-isolated attempt would overwrite it.
func removePerJobSourceDir(jobID int64) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(home, ".cache", "weft", "jobs", fmt.Sprintf("%d", jobID)))
}

// removePerJobSourceMarker deletes the per-job .weft-source.<jobid>.sha256
// file from the working directory. Best-effort: a leftover marker is
// harmless (the next dispatch overwrites it).
func removePerJobSourceMarker(workingDir string, jobID int64) {
	if workingDir == "" {
		return
	}
	expanded := workingDir
	if len(expanded) > 1 && expanded[0] == '~' {
		if home, err := os.UserHomeDir(); err == nil {
			expanded = home + expanded[1:]
		}
	}
	_ = os.Remove(filepath.Join(expanded, srcsync.PerJobSourceMarkerFile(jobID)))
}

func (r *Runner) refreshRunningJobs() {
	changed := false
	for _, jobIDStr := range r.state.RunningIDs() {
		jobID := mustParseInt64(jobIDStr)
		paths := NewJobPaths(r.logDir, jobID)

		// Check if status file appeared (job completed outside our wait goroutine).
		// This covers two cases: (1) the bash wrapper captured the exit code
		// after the runner restarted, (2) normal completion race with refresh.
		if _, err := os.Stat(paths.Status); err == nil {
			r.processesMu.Lock()
			_, hasProc := r.processes[jobIDStr]
			r.processesMu.Unlock()

			if !hasProc {
				exitCode, _ := ReadStatusFile(paths.Status)
				ei := ExitInfo{ExitCode: exitCode}
				endTime := time.Now().Unix()
				rs, _ := r.state.GetRunning(jobIDStr)
				if exitCode == 0 {
					oplog.LogJob(oplog.OpJobComplete, jobID, "", oplog.WithDetail("exit=0 (recovered)"))
					slog.Info("job completed (recovered)", "component", "runner", "job_id", jobID)
				} else {
					failureReason := DetectFailureReasonFromExitInfo(ei)
					WriteFailureReasonFile(paths, failureReason)
					oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetailf("exit=%d (recovered)", exitCode))
					slog.Warn("job failed (recovered)", "component", "runner", "job_id", jobID, "exit_code", exitCode)
				}
				WriteCompletionRecord(paths, ei, rs, "", "", rs.StartedAt, endTime, nil)
				WriteRusageFile(paths, rs)
				r.state.RecordFinished(jobIDStr, exitCode, endTime)
				r.state.RemoveRunning(jobIDStr)
				CleanupPIDFiles(paths)
				changed = true
			}
			continue
		}

		// Check if process is stopped (state T) without .paused marker
		pgid, hasPGID := ReadPIDFile(paths.PGID)
		pid, hasPID := ReadPIDFile(paths.PID)

		checkPID := 0
		if hasPGID {
			checkPID = pgid
		} else if hasPID {
			checkPID = pid
		}

		if checkPID > 0 && CheckProcessStopped(checkPID) {
			if _, err := os.Stat(paths.Paused); err == nil {
				continue // Intentionally paused
			}
			fmt.Printf("Job %s process %d is stopped (state T) - marking as failed\n", ids.FormatJobID(jobID), checkPID)
			oplog.LogJob("job.stopped_detected", jobID, "", oplog.WithDetailf("pid=%d state=T", checkPID))

			WriteKillReasonFile(paths, KillReasonStoppedDetected)

			if hasPGID {
				KillProcessGroup(pgid)
			}
			if hasPID && pid != pgid {
				syscall.Kill(pid, syscall.SIGKILL)
			}

			stoppedEI := ExitInfo{ExitCode: 1}
			WriteStatusFile(paths, stoppedEI)
			oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetail("exit=1 reason=stopped"))
			rs, _ := r.state.GetRunning(jobIDStr)
			WriteRusageFile(paths, rs)
			endTime := time.Now().Unix()
			WriteCompletionRecord(paths, stoppedEI, rs, KillReasonStoppedDetected, "stopped", rs.StartedAt, endTime, nil)
			r.state.RecordFinished(jobIDStr, 1, endTime)
			r.state.RemoveRunning(jobIDStr)
			CleanupPIDFiles(paths)
			changed = true
			continue
		}

		// Check if wrapper process is still alive. Zombies (state Z) pass
		// kill -0 because the kernel keeps a process table entry, but they
		// will never write the status file — their parent (the agent) lost
		// its Wait() goroutine across a re-exec or never installed one.
		// Treat zombies as gone so the slot doesn't stay occupied forever.
		zombieWrapper := false
		if hasPID && CheckPIDAlive(pid) {
			if !CheckProcessZombie(pid) {
				continue
			}
			zombieWrapper = true
			fmt.Printf("Job %s wrapper pid %d is zombie - treating as gone\n", ids.FormatJobID(jobID), pid)
			oplog.LogJob("job.zombie_detected", jobID, "", oplog.WithDetailf("pid=%d state=Z", pid))
		}

		// If the waitForJob goroutine is still tracking this process,
		// it will handle the exit — don't treat it as an orphan.
		// This avoids a race where bash exits, Wait() returns, but
		// the status file hasn't been written yet when we check here.
		// Skip this guard for zombies: the wrapper is already reaped/dead
		// in any meaningful sense and Wait() will not produce a status file.
		if !zombieWrapper {
			r.processesMu.Lock()
			_, hasWaiter := r.processes[jobIDStr]
			r.processesMu.Unlock()
			if hasWaiter {
				continue
			}
		}

		// Wrapper gone — kill orphaned process group
		if hasPGID && CheckPIDAlive(pgid) {
			WriteKillReasonFile(paths, KillReasonOrphan)
			fmt.Printf("Killing orphaned process group %d for job %s\n", pgid, ids.FormatJobID(jobID))
			KillProcessGroup(pgid)
			oplog.LogJob("job.orphan_killed", jobID, "", oplog.WithDetailf("pgid=%d", pgid))
		}

		// Reap the zombie wrapper (if any) so the process table entry clears.
		// Best-effort: if syscall.Exec preserved the parent-child relation
		// we should succeed; if Wait returns ECHILD we silently move on.
		if zombieWrapper {
			reapZombie(pid)
		}

		// Re-check for a status file before declaring the job orphaned. The
		// wrapper's SIGTERM trap (WrapCommandWithExitCapture) writes the
		// status file at process exit, and the gap between the top-of-loop
		// stat at paths.Status and this point is wide enough for the trap
		// to land. If a real status file arrived, use the existing recovery
		// path (read its exit code) rather than clobbering it with a
		// synthetic ExitCode: 1.
		if exitCode, ok := ReadStatusFile(paths.Status); ok {
			ei := ExitInfo{ExitCode: exitCode}
			endTime := time.Now().Unix()
			rs, _ := r.state.GetRunning(jobIDStr)
			if exitCode == 0 {
				oplog.LogJob(oplog.OpJobComplete, jobID, "", oplog.WithDetail("exit=0 (recovered after orphan check)"))
			} else {
				failureReason := DetectFailureReasonFromExitInfo(ei)
				WriteFailureReasonFile(paths, failureReason)
				oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetailf("exit=%d (recovered after orphan check)", exitCode))
			}
			WriteCompletionRecord(paths, ei, rs, "", "", rs.StartedAt, endTime, nil)
			WriteRusageFile(paths, rs)
			r.state.RecordFinished(jobIDStr, exitCode, endTime)
			r.state.RemoveRunning(jobIDStr)
			CleanupPIDFiles(paths)
			changed = true
			continue
		}

		// No status file — truly orphaned, exit code unknown
		orphanEI := ExitInfo{ExitCode: 1}
		WriteStatusFile(paths, orphanEI)
		oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetail("exit=1 duration=0"))
		endTime := time.Now().Unix()
		rs, _ := r.state.GetRunning(jobIDStr)
		WriteCompletionRecord(paths, orphanEI, rs, KillReasonOrphan, KillReasonOrphan, rs.StartedAt, endTime, nil)
		WriteRusageFile(paths, rs)
		r.state.RecordFinished(jobIDStr, 1, endTime)
		r.state.RemoveRunning(jobIDStr)
		CleanupPIDFiles(paths)
		changed = true
	}

	if changed {
		r.saveState()
	}
}

func (r *Runner) sampleRunningJobs() {
	now := time.Now()
	updated := false

	for _, jobIDStr := range r.state.RunningIDs() {
		jobID := mustParseInt64(jobIDStr)
		paths := NewJobPaths(r.logDir, jobID)

		rs, ok := r.state.GetRunning(jobIDStr)
		if !ok {
			continue
		}
		if rs.TelemetryIntervalSeconds <= 0 {
			rs.TelemetryIntervalSeconds = int64(DefaultJobTelemetryPolicy().Interval / time.Second)
			rs.TelemetryAdvancedGPU = DefaultJobTelemetryPolicy().CollectAdvancedGPU
		}
		if rs.LastSample > 0 && now.Unix()-rs.LastSample < rs.TelemetryIntervalSeconds {
			continue
		}

		pid, ok := ReadPIDFile(paths.PID)
		if !ok || !CheckPIDAlive(pid) {
			continue
		}

		// Resolve PGID for resource usage
		pgid := 0
		if pg, ok := ReadPIDFile(paths.PGID); ok {
			pgid = pg
		}

		// Collect telemetry sample
		oldPressure := rs.PeakMemPressure
		pressure, _ := SampleJob(pid, pgid, r.cpuCount, paths, &rs, "multi")

		// Log pressure escalation
		if MemPressureSeverity(pressure) > MemPressureSeverity(oldPressure) {
			if oldPressure != "" && oldPressure != MemPressureNormal {
				oplog.LogJob("job.mem_pressure", jobID, "", oplog.WithDetailf("level=%s", pressure))
			}
		}

		r.state.SetRunning(jobIDStr, rs)
		updated = true
	}

	if updated {
		r.saveState()
	}
}

func (r *Runner) adjustRunningJobAllotments() {
	now := time.Now()
	updated := false

	for _, jobIDStr := range r.state.RunningIDs() {
		jobID := mustParseInt64(jobIDStr)
		paths := NewJobPaths(r.logDir, jobID)

		rs, ok := r.state.GetRunning(jobIDStr)
		if !ok || rs.WarmupUntil > now.Unix() {
			continue
		}

		pid, ok := ReadPIDFile(paths.PID)
		if !ok || !CheckPIDAlive(pid) {
			continue
		}

		hostPct := ProcCPUHostPct(pid, r.cpuCount)
		rs.Samples = appendBounded(rs.Samples, hostPct, r.cpuConfig.SampleCount())

		newAllotment, newOverHist, newUnderHist := r.cpuConfig.AdjustAllotment(
			rs.LocalAllotment, rs.Samples, rs.OverHist, rs.UnderHist, r.state.TotalAllotment())
		if newAllotment != rs.LocalAllotment {
			if newAllotment > rs.LocalAllotment {
				oplog.LogJob("job.allotment_increase", jobID, "", oplog.WithDetailf("cpu=%d", newAllotment))
			} else {
				oplog.LogJob("job.allotment_decay", jobID, "", oplog.WithDetailf("cpu=%d", newAllotment))
			}
			rs.LocalAllotment = newAllotment
			rs.Samples = nil
		}
		rs.OverHist = newOverHist
		rs.UnderHist = newUnderHist

		r.state.SetRunning(jobIDStr, rs)
		updated = true
	}

	if updated {
		r.saveState()
	}
}

// saveState saves the runner state to disk, logging any error.
// All callers use this instead of r.state.Save directly so that
// write failures (e.g. NFS unavailability) are never silently swallowed.
func (r *Runner) saveState() {
	if err := r.state.Save(r.stateFile); err != nil {
		fmt.Fprintf(os.Stderr, "warning: save state: %v\n", err)
		oplog.Log("queue.save_state_failed", oplog.WithError(err))
	}
}

func tickerChan(ticker *time.Ticker) <-chan time.Time {
	if ticker == nil {
		return nil
	}
	return ticker.C
}

func (r *Runner) warmupActive() bool {
	now := time.Now().Unix()
	for _, rs := range r.state.RunningSnapshot() {
		if rs.WarmupUntil > now {
			return true
		}
	}
	return false
}

func (r *Runner) jobAllotment(job *opsqueue.CommandJob) int {
	// Check explicit CPU field
	if job.CPU != nil && *job.CPU > 0 {
		return *job.CPU
	}
	// GPU jobs get a lower default (GPU-bound, need fewer CPU cores)
	if job.GPU != "" || job.GPUClass != "" || job.GPUMem != nil {
		return r.cpuConfig.DefaultGPUAllotment(r.cpuCount)
	}
	return r.cpuConfig.DefaultAllotment(r.cpuCount)
}

func (r *Runner) updateCurrentFile() {
	current, ok := r.state.CurrentJobID()
	if !ok {
		os.Remove(r.currentFile)
	} else {
		os.WriteFile(r.currentFile, []byte(fmt.Sprintf("%d\n", current)), 0644)
	}
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s\n", line)
}
