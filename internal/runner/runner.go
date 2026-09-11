package runner

import (
	"context"
	"errors"
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
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
	"github.com/osteele/weft/internal/inventory"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/opsqueue"
	srcsync "github.com/osteele/weft/internal/sync"
)

// Runner is the main queue runner that manages job execution.
type Runner struct {
	queueDir     string
	logDir       string
	setupTimeout time.Duration

	state            *State
	cmdProc          *CommandProcessor
	gpuInv           *GPUInventory
	cpuConfig        CPUConfig
	telemetryConfig  TelemetryConfig
	benchCfg         BenchmarkConfig
	cpuCount         int
	ramTargetPercent int
	hostMemoryStats  func() (totalKB, usedKB, availableKB int64)
	processRSSKB     func(pid int) int64
	observeJobCPU    func(jobID int64, cpuCount int) (hostPct int, ok bool)

	// File paths
	commandsFile string
	stateFile    string
	currentFile  string
	pidFile      string
	runnerLog    string

	// Runtime state
	processes     map[string]*Process // jobID -> process
	hookStopFuncs map[string]func()   // jobID -> stop function from OnJobStart
	// finalizingWorkdirs gates reuse while a terminal attempt is being handed
	// to the post-job manager. It is deliberately separate from Running: a
	// blocked publication admission must not retain CPU/GPU scheduler capacity.
	finalizingWorkdirs map[string]int
	processesMu        sync.Mutex
	lastSampleTime     time.Time
	restartRequested   bool
	restartEnv         []string
	nowFunc            func() time.Time
	AgentVersion       string
	Capabilities       []string

	// Benchmark tracking
	benchmarkIdleCount  int
	benchmarkLastReason string

	// Lifecycle hooks (optional, best-effort)
	OnJobStart            func(jobID, runID int64, logPath string) func() // returns stop function for live upload
	OnJobFinish           func(jobID, runID int64, logDir string, exitCode int)
	RecoverJobPublication func(context.Context, PostJobCapture)

	// PostJobManager coordinates post-job artifact capture with subsequent
	// starts. Runners call WaitForWorkdir before starting a job, then
	// StartPostJob after the job's completion record has been written.
	PostJobManager PostJobManager

	// EnsureSourceFromR2 is an optional preflight hook used by jobs queued
	// in R2-isolated mode (CommandJob.SourceR2Key != ""). The agent
	// implementation downloads the exact recorded v1 or v2 source key from
	// R2 (rclone) and extracts it into perJobDir, then returns nil. A non-nil
	// error causes the runner to reject the attempt at preflight (no startTime,
	// no meta file, sentinel + failure_reason written). When nil, jobs with
	// SourceR2Key fall back to the shared working dir with a debug log.
	EnsureSourceFromR2 func(jobID int64, r2Key, perJobDir string) error

	// EnsureSourceManifestFromR2 materializes a complete submit-time source
	// closure and returns its project working directory. It takes precedence
	// over the legacy single-tarball hook when SourceManifest is present.
	EnsureSourceManifestFromR2 func(jobID int64, manifest opsqueue.SourceManifest, perJobRoot string) (string, error)
	// EnsurePayloadsFromR2 stages logical-job input artifacts and returns the
	// owner-private directory to expose as WEFT_PAYLOAD_DIR.
	EnsurePayloadsFromR2 func(jobID int64, payloads []opsqueue.Payload) (string, error)
	// EnsureArtifactNeedsFromR2 stages controller-resolved dependencies into the
	// runtime working directory after any isolated source has been materialized.
	EnsureArtifactNeedsFromR2 func(jobID int64, workDir string, needs []opsqueue.ArtifactNeed) error

	// Shutdown
	stopCh chan struct{}
}

type PostJobCapture struct {
	JobID    int64
	RunID    int64
	WorkDir  string
	LogDir   string
	ExitCode int
	// StartTime is the attempt's start (unix seconds); post-job output
	// uploads window their walk to files modified at or after it.
	StartTime int64
	// OutputDirs carries the job's configured convention output dirs so
	// post-job uploads walk the same dirs discovery attributes.
	OutputDirs []string
	// CleanupDir remains available to the post-job manager as an upload source
	// and is removed only after publication finishes. This prevents an
	// R2-isolated job's per-job source tree from disappearing while a bounded
	// publication item is queued.
	CleanupDir string
}

type PostJobManager interface {
	WaitForWorkdir(workdir string)
	WaitForAll()
	StartPostJob(capture PostJobCapture)
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
	runner := &Runner{
		queueDir:           cfg.QueueDir,
		logDir:             cfg.LogDir,
		setupTimeout:       cfg.SetupTimeout,
		commandsFile:       filepath.Join(cfg.QueueDir, opsqueue.CommandsFileName()),
		stateFile:          filepath.Join(cfg.QueueDir, opsqueue.StateFileName()),
		currentFile:        filepath.Join(cfg.QueueDir, opsqueue.CurrentFileName()),
		pidFile:            filepath.Join(cfg.QueueDir, opsqueue.PidFileName()),
		runnerLog:          filepath.Join(cfg.QueueDir, opsqueue.RunnerLogName()),
		cpuConfig:          DefaultCPUConfig(),
		telemetryConfig:    DefaultTelemetryConfig(),
		benchCfg:           DefaultBenchmarkConfig(),
		ramTargetPercent:   defaultRAMUtilizationTarget,
		hostMemoryStats:    HostMemoryStatsKB,
		processRSSKB:       ProcCurrentRSSKB,
		processes:          make(map[string]*Process),
		hookStopFuncs:      make(map[string]func()),
		finalizingWorkdirs: make(map[string]int),
		nowFunc:            time.Now,
		stopCh:             make(chan struct{}),
	}
	runner.observeJobCPU = runner.observeRunningJobCPU
	return runner
}

func (r *Runner) now() time.Time {
	if r.nowFunc != nil {
		return r.nowFunc()
	}
	return time.Now()
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
	r.state.SetCapabilities(r.Capabilities)
	r.state.SetAgentIdentity(r.AgentVersion, opsqueue.QueueProtocolVersion)
	r.saveState()

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

	// Recover retained terminal evidence before consuming retry commands that
	// may archive the original attempt's files.
	if r.RecoverJobPublication != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		r.recoverRetainedPublications(ctx)
		cancel()
	}
	return r.mainLoop()
}

func (r *Runner) recoverRetainedPublications(ctx context.Context) {
	visited := 0
	for idText := range r.state.Finished {
		if err := ctx.Err(); err != nil {
			slog.Warn("startup publication recovery budget exhausted; retained records remain for a later recovery",
				"remaining", len(r.state.Finished)-visited, "error", err)
			return
		}
		visited++
		jobID, err := strconv.ParseInt(idText, 10, 64)
		if err != nil {
			slog.Warn("invalid finished job ID", "job_id", idText, "error", err)
			continue
		}
		rec, err := ReadCompletionRecord(NewJobPaths(r.logDir, jobID))
		if err != nil {
			slog.Warn("read retained completion for publication recovery", "job_id", jobID, "error", err)
			continue
		}
		if rec.RunID > 0 && rec.EndTime > 0 {
			if rec.OutputDirs == nil {
				slog.Warn("retained completion has no output-directory metadata; recovering conventional outputs only",
					"job_id", jobID, "run_id", rec.RunID)
			}
			r.RecoverJobPublication(ctx, PostJobCapture{
				JobID: jobID, RunID: rec.RunID, WorkDir: rec.RuntimeWorkingDir,
				LogDir: r.logDir, ExitCode: rec.ExitCode, StartTime: rec.StartTime,
				OutputDirs: rec.OutputDirs,
			})
		}
	}
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

var execRunner = syscall.Exec

func (r *Runner) tick() error {
	// Process new commands
	result, err := r.cmdProc.ProcessCommands(r.state)
	if err != nil {
		fmt.Fprintf(os.Stderr, "process commands: %v\n", err)
	}

	for _, capture := range result.RecoveredCompletions {
		if r.RecoverJobPublication != nil {
			r.RecoverJobPublication(context.Background(), capture)
		}
	}
	if result.RestartRequested {
		r.restartRequested = true
		r.restartEnv = result.RestartEnv
	}
	if r.restartRequested {
		// Hold the finalization lock through exec. A waiter that has not yet
		// entered finalization retains its Running entry for recovery.
		r.processesMu.Lock()
		if len(r.finalizingWorkdirs) > 0 {
			r.processesMu.Unlock()
			r.saveState()
			return nil
		}
		r.saveState()
		oplog.Log("cmd.restart")
		exe, err := os.Executable()
		if err == nil {
			err = execRunner(exe, os.Args, mergeEnvVars(os.Environ(), r.restartEnv))
		}
		r.processesMu.Unlock()
		r.restartRequested = false
		r.restartEnv = nil
		if err != nil {
			oplog.Log("cmd.restart.failed", oplog.WithError(err))
			slog.Error("runner re-exec failed; continuing supervision", "error", err)
		}
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

	pending := r.state.PendingSnapshot()
	if len(pending) == 0 {
		return
	}

	headDecision, ok := r.evaluatePendingJob(pending[0], true)
	if !ok {
		r.requeueUnreadablePending(pending[0])
		return
	}
	if headDecision.benchmarkIdleReason != "" {
		r.markBenchmarkIdleWait(headDecision)
		if r.tryStartAfterGatedBenchmark(pending[1:]) {
			r.saveState()
			return
		}
		r.saveState()
		return
	}

	r.applyPendingDecision(headDecision)
}

type pendingStartDecision struct {
	jobID               int64
	job                 *opsqueue.CommandJob
	rj                  *RunnerJob
	canStart            bool
	reason              string
	benchmarkIdleReason string
	resolvedGPUDevices  []string
	waitedForPostJob    bool
}

func (r *Runner) evaluatePendingJob(jobID int64, waitForPostJob bool) (pendingStartDecision, bool) {
	job, err := ReadJobFile(r.queueDir, jobID)
	if err != nil {
		return pendingStartDecision{}, false
	}
	return r.evaluateLoadedPendingJob(jobID, job, waitForPostJob), true
}

func (r *Runner) evaluateLoadedPendingJob(jobID int64, job *opsqueue.CommandJob, waitForPostJob bool) pendingStartDecision {
	rj := &RunnerJob{Data: job, ID: jobID}
	decision := pendingStartDecision{
		jobID: jobID,
		job:   job,
		rj:    rj,
	}

	if r.PostJobManager != nil && r.postJobFinalizationPending(job) {
		decision.reason = "post-job gate: prior terminal attempt is still entering publication"
		return decision
	}

	if waitForPostJob && r.PostJobManager != nil {
		if HasBenchmarkTag(rj) {
			r.PostJobManager.WaitForAll()
		} else {
			r.PostJobManager.WaitForWorkdir(ExpandTilde(job.Dir))
		}
		decision.waitedForPostJob = true
	}

	// Check exclusive constraints.
	if AnyRunningExclusive(r.state, r.queueDir) {
		decision.reason = "exclusive gate: waiting for another exclusive job to finish"
		return decision
	}

	runningCount := r.state.RunningCount()
	if HasExclusiveOrBenchmarkTag(rj) && runningCount > 0 {
		decision.reason = fmt.Sprintf("exclusive gate: waiting for %d running job(s) to finish", runningCount)
		if HasBenchmarkTag(rj) {
			decision.reason = fmt.Sprintf("benchmark gate: waiting for %d running job(s) to finish", runningCount)
		}
		return decision
	}

	// Benchmark: wait for system idle.
	if HasBenchmarkTag(rj) {
		reason := r.benchCfg.SystemIdleCheck()
		if reason != "" {
			decision.reason = "benchmark gate: " + reason
			decision.benchmarkIdleReason = reason
			return decision
		}
	}

	// Check CPU capacity.
	if runningCount > 0 {
		currentAllotment := r.state.TotalAllotment()
		nextAllotment := r.jobAllotment(job)
		if currentAllotment+nextAllotment > r.cpuConfig.HostUtilizationTarget {
			decision.reason = fmt.Sprintf("cpu gate: %d%% + %d%% > %d%% target", currentAllotment, nextAllotment, r.cpuConfig.HostUtilizationTarget)
			return decision
		}
	}

	// Host-wide measured load gate. Independent of weft's allotment
	// bookkeeping; catches overload from non-weft users on shared hosts,
	// BLAS/OMP overdraft beyond declared cores, and any other source of
	// load the per-job CPU allotment system has no visibility into.
	if r.cpuConfig.HostLoadCeiling > 0 && r.cpuCount > 0 {
		loadPct := int((HostLoadAvg1() * 100.0) / float64(r.cpuCount))
		if loadPct >= r.cpuConfig.HostLoadCeiling {
			decision.reason = fmt.Sprintf("host load gate: %d%% >= %d%% ceiling (1-min loadavg / %d cores)", loadPct, r.cpuConfig.HostLoadCeiling, r.cpuCount)
			return decision
		}
	}

	if ramDecision := r.evaluateRAMAdmission(job); !ramDecision.Admit {
		decision.reason = fmt.Sprintf(
			"ram gate: projected %.1f GiB > %.1f GiB target",
			float64(ramDecision.ProjectedUsedKB)/(1024*1024),
			float64(ramDecision.CapacityKB)/(1024*1024),
		)
		return decision
	}

	// Refresh actual GPU memory snapshot before GPU checks (only for GPU jobs).
	jobHasGPU := job.GPUClass != "" || len(GetJobGPUDevices(job)) > 0
	if jobHasGPU {
		if r.gpuInv == nil {
			decision.reason = "gpu gate: inventory unavailable"
			return decision
		}
		r.gpuInv.RefreshDeviceMemSnapshot()
	}

	// Check GPU capacity and resolve devices.
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
			decision.reason = gpuReason
			return decision
		}
	}

	// Auto-assign GPU on hosts with GPUs when the job has no GPU constraints.
	// Without this, CUDA defaults to GPU 0, causing OOM when GPU 0 is loaded.
	if !jobHasGPU && r.gpuInv != nil && len(r.gpuInv.Devices) > 0 {
		r.gpuInv.RefreshDeviceMemSnapshot()
		if device := r.gpuInv.PickLeastLoadedGPU(r.state); device != "" {
			resolvedGPUDevices = []string{device}
		}
	}

	decision.canStart = true
	decision.resolvedGPUDevices = resolvedGPUDevices
	return decision
}

func (r *Runner) requeueUnreadablePending(jobID int64) {
	// Pop the next job
	poppedID, ok := r.state.PopPending()
	if !ok {
		return
	}
	if poppedID != jobID {
		r.state.AddPending(poppedID)
		return
	}

	// Load job data
	_, err := ReadJobFile(r.queueDir, jobID)
	if err != nil {
		reason := fmt.Sprintf("missing queue payload: %v", err)
		fmt.Fprintf(os.Stderr, "Job %s: cannot read job file: %v\n", ids.FormatJobID(jobID), err)
		r.state.AddPendingWithReason(jobID, reason)
		r.saveState()
		return
	}
}

func (r *Runner) applyPendingDecision(decision pendingStartDecision) {
	jobID := decision.jobID
	job := decision.job
	rj := decision.rj

	if !decision.canStart {
		if _, ok := r.state.PopPending(); !ok {
			return
		}
		r.state.AddPendingWithReason(jobID, decision.reason)
		r.saveState()
		return
	}

	// Benchmark: confirm consecutive idle samples before starting.
	if HasBenchmarkTag(rj) {
		r.benchmarkIdleCount++
		if r.benchmarkIdleCount < r.benchCfg.IdleSamples {
			if _, ok := r.state.PopPending(); !ok {
				return
			}
			r.state.AddPendingWithReason(jobID, fmt.Sprintf("benchmark gate: confirming idle (%d/%d)", r.benchmarkIdleCount, r.benchCfg.IdleSamples))
			r.saveState()
			return
		}
		oplog.LogJob("benchmark.idle_confirmed", jobID, "", oplog.WithDetailf("samples=%d", r.benchmarkIdleCount))
		r.benchmarkIdleCount = 0
		r.benchmarkLastReason = ""
	}

	r.state.RemovePending(jobID)

	// Wait for prior post-job uploads from this workdir before reusing it.
	if r.PostJobManager != nil && !decision.waitedForPostJob {
		if HasBenchmarkTag(rj) {
			r.PostJobManager.WaitForAll()
		} else {
			r.PostJobManager.WaitForWorkdir(ExpandTilde(job.Dir))
		}
	}

	r.startPendingJob(decision)
}

func (r *Runner) markBenchmarkIdleWait(decision pendingStartDecision) {
	reason := decision.benchmarkIdleReason
	if reason != r.benchmarkLastReason {
		oplog.LogJob("benchmark.waiting", decision.jobID, "", oplog.WithDetail(reason))
		r.benchmarkLastReason = reason
	}
	r.benchmarkIdleCount = 0
	r.state.SetPendingReason(decision.jobID, decision.reason)
}

func (r *Runner) tryStartAfterGatedBenchmark(candidateIDs []int64) bool {
	for _, jobID := range candidateIDs {
		job, err := ReadJobFile(r.queueDir, jobID)
		if err != nil {
			continue
		}
		if HasBenchmarkTag(&RunnerJob{Data: job, ID: jobID}) {
			continue
		}
		decision := r.evaluateLoadedPendingJob(jobID, job, false)
		if !decision.canStart {
			continue
		}
		if depResult := checkJobDependencies(decision.job, r.logDir); depResult.Result != DepOK {
			continue
		}
		r.state.RemovePending(jobID)
		if r.PostJobManager != nil {
			r.PostJobManager.WaitForWorkdir(ExpandTilde(decision.job.Dir))
		}
		r.startPendingJob(decision)
		return true
	}
	return false
}

func (r *Runner) startPendingJob(decision pendingStartDecision) {
	jobID := decision.jobID
	job := decision.job
	// Start the job
	slog.Info("job starting", "component", "runner", "job_id", jobID, "running_count", r.state.RunningCount(), "gpu_class", job.GPUClass, "resolved_gpu", decision.resolvedGPUDevices)
	err := r.startJob(jobID, job, decision.resolvedGPUDevices)
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

// RetryablePreflightError marks an infrastructure failure that happened before
// the user command started and can be retried against the same accepted job.
type RetryablePreflightError struct {
	err error
}

func (e *RetryablePreflightError) Error() string {
	return e.err.Error()
}

func (e *RetryablePreflightError) Unwrap() error {
	return e.err
}

// MarkRetryablePreflight preserves an accepted job when source acquisition
// fails transiently instead of recording a terminal preflight rejection.
func MarkRetryablePreflight(err error) error {
	if err == nil {
		return nil
	}
	return &RetryablePreflightError{err: err}
}

func isRetryablePreflight(err error) bool {
	var target *RetryablePreflightError
	return errors.As(err, &target)
}

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

type processDeadlineState struct {
	mu            sync.Mutex
	timer         *time.Timer
	processExited bool
	timedOut      bool
}

func (s *processDeadlineState) start(timeout time.Duration, onTimeout func()) {
	if timeout <= 0 {
		return
	}
	s.timer = time.AfterFunc(timeout, func() {
		s.mu.Lock()
		if s.processExited {
			s.mu.Unlock()
			return
		}
		s.timedOut = true
		s.mu.Unlock()
		onTimeout()
	})
}

func (s *processDeadlineState) finish() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	s.processExited = true
	timedOut := s.timedOut
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
	return timedOut
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
	depResult := checkJobDependencies(job, r.logDir)
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
	wallStartedAt := time.Now()
	if requested := requestedGPUCount(job); requested > 1 && len(gpuDevices) < requested {
		detail := fmt.Sprintf("gpu_count_preflight_failed: requested=%d visible=%d", requested, len(gpuDevices))
		return r.rejectPreflight(job, paths, db.FailureReasonGPUCountPreflightFailed, detail)
	}

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

	// Pinned jobs materialize and verify their complete submit-time source
	// closure. Legacy Layer D jobs materialize one content-addressed tarball.
	// Both run from per-job directories and bypass the shared-tree marker.
	var sourceExecution *SourceExecutionMetadata
	if job.SourceManifest != nil {
		if r.EnsureSourceManifestFromR2 == nil {
			detail := fmt.Sprintf("pinned_source_unavailable: source_manifest=%s but the runner has no manifest materializer", job.SourceManifest.SHA256)
			return r.rejectPreflight(job, paths, db.FailureReasonPinnedSourceUnavailable, detail)
		}
		perJobRoot := perJobSourceDir(jobID)
		manifestDir, err := r.EnsureSourceManifestFromR2(jobID, *job.SourceManifest, perJobRoot)
		if err != nil {
			if isRetryablePreflight(err) {
				slog.Warn("pinned source fetch will retry", "component", "runner", "job_id", jobID, "error", err)
				return errRequeue
			}
			return r.rejectPreflight(job, paths, db.FailureReasonPinnedSourceFetchFailed, fmt.Sprintf("pinned_source_fetch_failed: %v", err))
		}
		expandedDir = manifestDir
		sourceExecution = &SourceExecutionMetadata{
			DispatchMode:     "pinned_inventory_manifest",
			IdentityKind:     "source_manifest_v2",
			DispatchedSHA256: job.SourceManifest.SHA256,
			VerifiedSHA256:   job.SourceManifest.SHA256,
			Verification:     "verified",
			VerifiedAt:       r.now().Unix(),
			AgentVersion:     r.AgentVersion,
			RootCount:        len(job.SourceManifest.Roots),
		}
	} else if job.SourceR2Key != "" {
		if r.EnsureSourceFromR2 == nil {
			detail := fmt.Sprintf("r2_isolated_source_unavailable: SourceR2Key=%s but the runner has no EnsureSourceFromR2 hook", job.SourceR2Key)
			return r.rejectPreflight(job, paths, db.FailureReasonR2IsolatedSourceUnavailable, detail)
		}
		perJobDir := perJobSourceDir(jobID)
		if err := r.EnsureSourceFromR2(jobID, job.SourceR2Key, perJobDir); err != nil {
			if isRetryablePreflight(err) {
				slog.Warn("isolated source fetch will retry", "component", "runner", "job_id", jobID, "error", err)
				return errRequeue
			}
			return r.rejectPreflight(job, paths, db.FailureReasonR2IsolatedSourceFetchFailed, fmt.Sprintf("r2_isolated_source_fetch_failed: %v", err))
		}
		expandedDir = perJobDir
		sourceExecution = &SourceExecutionMetadata{
			DispatchMode:     "legacy_r2_tarball",
			IdentityKind:     "canonical_tar_sha256",
			DispatchedSHA256: job.SourceSHA,
			Verification:     "legacy-unverifiable",
			AgentVersion:     r.AgentVersion,
			RootCount:        1,
		}
	} else if job.SourceSHA != "" {
		// Preflight (Layer A+C): per-job source provenance check. Runs
		// before startTime is stamped so a rejection isn't recorded as a
		// 0-second "completed" attempt. On failure: write the
		// failure_reason file (read by the agent's batch-status) and the
		// preflight_rejected sentinel (signals to sync that
		// this attempt never started), then return.
		markerSHA, err := srcsync.ReadSourceMarkerForJob(expandedDir, jobID)
		if err != nil {
			msg := fmt.Sprintf("source_provenance_mismatch: expected=%s, marker unreadable in %s (%v)", job.SourceSHA, expandedDir, err)
			return r.rejectPreflight(job, paths, db.FailureReasonSourceProvenanceMismatch, msg)
		}
		if markerSHA != job.SourceSHA {
			msg := fmt.Sprintf("source_provenance_mismatch: expected=%s, marker=%s", job.SourceSHA, markerSHA)
			return r.rejectPreflight(job, paths, db.FailureReasonSourceProvenanceMismatch, msg)
		}
		sourceExecution = &SourceExecutionMetadata{
			DispatchMode:     "live_rsync_marker",
			IdentityKind:     "canonical_tar_sha256",
			DispatchedSHA256: job.SourceSHA,
			VerifiedSHA256:   markerSHA,
			Verification:     "verified",
			VerifiedAt:       r.now().Unix(),
			AgentVersion:     r.AgentVersion,
			RootCount:        1,
		}
	}

	artifactNeedsStaged := false
	if len(job.ArtifactNeeds) > 0 {
		if r.EnsureArtifactNeedsFromR2 == nil {
			return r.rejectPreflight(job, paths, db.FailureReasonArtifactStageFailed, "artifact staging unavailable: runner has no R2 artifact materializer")
		}
		if err := r.EnsureArtifactNeedsFromR2(jobID, expandedDir, job.ArtifactNeeds); err != nil {
			return r.rejectPreflight(job, paths, db.FailureReasonArtifactStageFailed, fmt.Sprintf("artifact staging failed: %v", err))
		}
		if err := writeArtifactNeedSatisfiedMarkers(r.logDir, job.ArtifactNeeds); err != nil {
			return r.rejectPreflight(job, paths, db.FailureReasonArtifactStageFailed, fmt.Sprintf("artifact marker write failed: %v", err))
		}
		artifactNeedsStaged = true
	}

	payloadDir := ""
	if len(job.Payloads) > 0 {
		if r.EnsurePayloadsFromR2 == nil {
			return r.rejectPreflight(job, paths, db.FailureReasonArtifactStageFailed, "payload staging unavailable: runner has no R2 payload materializer")
		}
		stagedDir, payloadErr := r.EnsurePayloadsFromR2(jobID, job.Payloads)
		if payloadErr != nil {
			return r.rejectPreflight(job, paths, db.FailureReasonArtifactStageFailed, fmt.Sprintf("payload staging failed: %v", payloadErr))
		}
		payloadDir = stagedDir
	}

	startTime := r.now().Unix()

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
	WriteMetaFile(paths, jobID, job.Dir, command, job.Desc, startTime, job.SourceSHA, sourceExecution)

	// Write log header
	WriteLogHeader(paths, jobID, job.Dir, command, job.SourceSHA)

	// Call OnJobStart hook (best-effort)
	if r.OnJobStart != nil {
		if stopFn := r.OnJobStart(jobID, job.RunID, paths.Log); stopFn != nil {
			r.processesMu.Lock()
			r.hookStopFuncs[jobIDStr] = stopFn
			r.processesMu.Unlock()
		}
	}

	// Write gpu_devices to meta file so sync can discover them
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
		if reason := SetupSkipReason(setupCmd, expandedDir, command, scriptMeta); reason != "" {
			slog.Info("skipping project uv sync",
				"component", "runner", "job_id", jobID, "reason", reason)
			appendSetupLog(paths.Log, []byte("weft: skipping project uv sync: "+reason+"\n"))
			setupCmd = ""
		}
	}
	if setupCmd == direnvSetupCommand {
		timeout, wallLimited, expired := boundedPhaseTimeout(r.setupTimeout, inventory.DefaultSetupTimeout, wallStartedAt, time.Duration(job.WallTimeSeconds)*time.Second)
		if expired {
			ei := ExitInfo{ExitCode: 124}
			appendSetupLog(paths.Log, []byte("weft: job wall time exceeded before direnv setup\n"))
			WriteStatusFile(paths, ei)
			r.finishFailedSetup(jobID, paths, ei, startTime, FailureReasonWallTimeout)
			return fmt.Errorf("job wall time exceeded before direnv setup")
		}
		resolvedEnv, ei, err := resolveDirenvEnv(expandedDir, envVars, paths.Log, timeout)
		if err != nil {
			WriteStatusFile(paths, ei)
			reason := ""
			if wallLimited && ei.ExitCode == 124 {
				reason = FailureReasonWallTimeout
			}
			r.finishFailedSetup(jobID, paths, ei, startTime, reason)
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
	if payloadDir != "" {
		envVars = append(envVars, "WEFT_PAYLOAD_DIR="+payloadDir)
	}
	if artifactNeedsStaged {
		envVars = append(envVars, "WEFT_ARTIFACT_NEEDS_STAGED=1")
	}

	// Inject resolved GPU device
	if len(gpuDevices) > 0 && job.GPUClass != "" {
		cudaEnv := FormatGPUDeviceEnv(gpuDevices)
		envVars = append(envVars, cudaEnv)
	}
	envVars = append(envVars, WeftGPUShapeEnv(job, gpuDevices)...)

	// Setup is part of the active attempt. Publish the running reservation
	// before entering the synchronous setup command so status transports do
	// not keep reporting the old pending snapshot for the duration of a slow
	// environment build.
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
		RAMReservationKB:         job.RAMReservationKB,
		DiskPath:                 expandedDir,
		OutputDirs:               job.OutputDirs,
		TelemetryIntervalSeconds: int64(telemetryPolicy.Interval / time.Second),
		TelemetryAdvancedGPU:     telemetryPolicy.CollectAdvancedGPU,
	})
	r.saveState()

	// Run environment setup as a separate phase. Use expandedDir, not
	// job.Dir: in R2-isolated mode the latter still points at the
	// user-facing dir while the extracted sources live under expandedDir.
	if setupCmd != "" {
		timeout, wallLimited, expired := boundedPhaseTimeout(r.setupTimeout, inventory.DefaultSetupTimeout, wallStartedAt, time.Duration(job.WallTimeSeconds)*time.Second)
		if expired {
			ei := ExitInfo{ExitCode: 124}
			appendSetupLog(paths.Log, []byte("weft: job wall time exceeded before setup\n"))
			WriteStatusFile(paths, ei)
			r.finishFailedSetup(jobID, paths, ei, startTime, FailureReasonWallTimeout)
			return fmt.Errorf("job wall time exceeded before setup")
		}
		ei, setupErr := RunSetupCommand(setupCmd, jobID, expandedDir, envVars, paths, timeout)
		if setupErr != nil {
			reason := ""
			if wallLimited && ei.ExitCode == 124 {
				reason = FailureReasonWallTimeout
			}
			r.finishFailedSetup(jobID, paths, ei, startTime, reason)
			return setupErr
		}
		if setupCmd == "uv sync" {
			collectAndWriteUVManifest(jobID, expandedDir, filepath.Dir(paths.Log))
		}
	}

	envVars = applyUVRunEnvAdditions(expandedDir, setupCmd, envVars, jobID, paths.Log)
	if job.WallTimeSeconds > 0 && time.Since(wallStartedAt) >= time.Duration(job.WallTimeSeconds)*time.Second {
		ei := ExitInfo{ExitCode: 124}
		appendSetupLog(paths.Log, []byte("weft: job wall time exceeded before command start\n"))
		WriteStatusFile(paths, ei)
		r.finishFailedSetup(jobID, paths, ei, startTime, FailureReasonWallTimeout)
		return fmt.Errorf("job wall time exceeded before command start")
	}
	appendSetupLog(paths.Log, []byte("weft: command starting\n"))

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
		r.state.RemoveRunning(jobIDStr)
		r.saveState()
		return fmt.Errorf("start process: %w", err)
	}

	// Write PID/PGID files
	slog.Debug("started process, writing PID files", "component", "runner", "job_id", jobID, "pid", proc.PID)
	proc.WritePIDFiles(paths)

	// Track the process
	r.processesMu.Lock()
	r.processes[jobIDStr] = proc
	r.processesMu.Unlock()

	deadlineState := &processDeadlineState{}
	if job.WallTimeSeconds > 0 {
		remaining := time.Duration(job.WallTimeSeconds)*time.Second - time.Since(wallStartedAt)
		deadlineState.start(remaining, func() {
			slog.Warn("wall-time reached, sending SIGTERM", "component", "runner", "job_id", jobID, "wall_time_seconds", job.WallTimeSeconds, "pgid", proc.PGID)
			KillProcessGroupWithGrace(proc.PGID, DefaultKillGrace, paths, "")
		})
	}

	// Start wait goroutine. Pass the resolved working dir (possibly the
	// per-job R2-isolated source dir) so output discovery and per-job
	// cleanup operate on the directory where the job actually ran.
	go r.waitForJob(jobID, proc, paths, startTime, rj, expandedDir, deadlineState)

	if r.telemetryConfig.Enabled {
		rs, _ := r.state.GetRunning(jobIDStr)
		SampleJob(proc.PID, proc.PGID, r.cpuCount, paths, &rs, "multi")
		r.state.UpdateRunningAttempt(jobIDStr, rs, rs)
	}

	return nil
}

func (r *Runner) evaluateRAMAdmission(job *opsqueue.CommandJob) RAMAdmissionDecision {
	totalKB, usedKB, _ := r.hostMemoryStats()
	running := make([]RAMCommitment, 0, r.state.RunningCount())
	for jobID, state := range r.state.RunningSnapshot() {
		reservationKB := state.RAMReservationKB
		if reservationKB == 0 {
			if queued, err := ReadJobFile(r.queueDir, mustParseInt64(jobID)); err == nil {
				reservationKB = queued.RAMReservationKB
			}
		}
		currentRSSKB := state.FinalRSSKB
		paths := NewJobPaths(r.logDir, mustParseInt64(jobID))
		if pid, ok := ReadPIDFile(paths.PID); ok {
			currentRSSKB = r.processRSSKB(pid)
		}
		running = append(running, RAMCommitment{
			ReservationKB: reservationKB,
			CurrentRSSKB:  currentRSSKB,
		})
	}
	return EvaluateRAMAdmission(RAMAdmissionInput{
		TotalKB:           totalKB,
		UsedKB:            usedKB,
		TargetPercent:     r.ramTargetPercent,
		Running:           running,
		NextReservationKB: job.RAMReservationKB,
	})
}

// finishFailedSetup closes out a job whose setup phase failed before the main
// process started, mirroring RunSingleJob's setup-failure handling: it writes
// the failure-reason file and completion record (so the reconciler sees a
// closed attempt rather than a half-started one), stops the OnJobStart hook,
// records the finished state, and removes the PID files and queue payload that
// would otherwise linger as debris. The status file has already been written
// by the setup path (RunSetupCommand or the direnv branch).
func (r *Runner) finishFailedSetup(jobID int64, paths JobPaths, ei ExitInfo, startTime int64, failureReason string) {
	jobIDStr := strconv.FormatInt(jobID, 10)
	if failureReason == "" {
		failureReason = DetectFailureReasonFromExitInfoAndLog(ei, paths.Log)
	}
	WriteFailureReasonFile(paths, failureReason)
	oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetailf("setup failed exit=%d reason=%s", ei.ExitCode, failureReason))

	endTime := r.now().Unix()
	if f, err := os.OpenFile(paths.Meta, os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		fmt.Fprintf(f, "end_time=%d\n", endTime)
		f.Close()
	}

	rs, _ := r.state.GetRunning(jobIDStr)
	killReason := ReadKillReasonFile(paths.KillReason)
	WriteCompletionRecord(paths, ei, rs, killReason, failureReason, startTime, endTime, nil)
	if r.state.FinishRunningAttempt(jobIDStr, rs, ei.ExitCode, endTime) {
		oplog.LogJob("job.slot_released", jobID, "", oplog.WithDetailf("run_id=%d exit=%d setup=true", rs.RunID, ei.ExitCode))
		r.saveState()
	}

	r.processesMu.Lock()
	stopFn := r.hookStopFuncs[jobIDStr]
	delete(r.hookStopFuncs, jobIDStr)
	r.processesMu.Unlock()
	CleanupPIDFiles(paths)
	removeJobFile(r.queueDir, jobID)
	if stopFn != nil {
		stopFn()
	}
	if r.OnJobFinish != nil {
		r.OnJobFinish(jobID, rs.RunID, filepath.Dir(paths.Log), ei.ExitCode)
	}
}

// rejectPreflight records a preflight rejection without ever marking the
// attempt as started. It writes the failure_reason file and a
// preflight_rejected sentinel (no status file, no meta, no log header), emits
// oplog.OpJobStartFailed, and removes the job from the runner queue so it
// won't be re-attempted on the next sweep. Reconciliation closes the attempt
// with its rejection time and failure reason, without a process start or exit.
func (r *Runner) rejectPreflight(job *opsqueue.CommandJob, paths JobPaths, reason, detail string) error {
	jobID := job.ID
	appendSetupLog(paths.Log, []byte("weft: "+detail+"\n"))
	if err := WriteFailureReasonFile(paths, reason); err != nil {
		slog.Warn("preflight reject: write failure_reason failed",
			"component", "runner", "job_id", jobID, "error", err)
	}
	if err := WritePreflightRejectedFile(paths); err != nil {
		slog.Warn("preflight reject: write sentinel failed",
			"component", "runner", "job_id", jobID, "error", err)
	}
	r.state.mu.Lock()
	if r.state.Rejected == nil {
		r.state.Rejected = make(map[string]opsqueue.RunnerRejectedState)
	}
	r.state.removePendingLocked(jobID)
	r.state.Rejected[strconv.FormatInt(jobID, 10)] = opsqueue.RunnerRejectedState{
		RunID: job.RunID, RejectedAt: r.now().Unix(), FailureReason: reason, Detail: detail,
	}
	r.state.mu.Unlock()
	r.saveState()
	oplog.LogJob(oplog.OpJobStartFailed, jobID, "", oplog.WithDetail(detail))
	removeJobFile(r.queueDir, jobID)
	return fmt.Errorf("preflight rejected: %s", detail)
}

func (r *Runner) waitForJob(jobID int64, proc *Process, paths JobPaths, startTime int64, rj *RunnerJob, runDir string, deadlineState *processDeadlineState) {
	jobIDStr := strconv.FormatInt(jobID, 10)
	err := proc.Cmd.Wait()
	wallTimedOut := deadlineState.finish()
	ei := ExtractExitInfo(err)
	if wallTimedOut {
		ei.ExitCode = 124
		ei.Signaled = true
		ei.Signal = syscall.SIGTERM
	}
	slog.Info("process exited", "component", "runner", "job_id", jobID, "exit_code", ei.ExitCode, "signaled", ei.Signaled, "error", err)

	endTime := r.now().Unix()
	duration := endTime - startTime

	// Write status and log footer. A wall deadline overrides the wrapper's
	// SIGTERM status (143) with the portable timeout status (124). Otherwise,
	// the wrapper may already have written both.
	if _, statErr := os.Stat(paths.Status); wallTimedOut || statErr != nil {
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
		if err := RecordDeclaredArtifacts(jobID, rj.Data.Produces, rj.Data.Outputs); err != nil {
			fmt.Fprintf(os.Stderr, "Job %s: failed to write artifact manifest from declared artifacts: %v\n", ids.FormatJobID(jobID), err)
			WriteManifestErrorFile(paths, "post-exit: "+err.Error())
		}
	}

	// On failure, detect the failure reason using signal-aware detection.
	var failureReason string
	if ei.ExitCode != 0 {
		if wallTimedOut {
			failureReason = FailureReasonWallTimeout
		} else {
			failureReason = DetectFailureReasonFromExitInfoAndLog(ei, paths.Log)
		}
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
		outputThreshold := AttemptOutputThreshold(startTime)
		if discovered, err := DiscoverJobOutputsSince(runDir, dirs, rj.Data.Outputs, outputThreshold); err == nil && len(discovered) > 0 {
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

	// Execution is terminal once the completion record exists. Release the
	// scheduler slot before any network publication, queue admission, log
	// draining, or cleanup callback can block. The attempt fence prevents a
	// delayed waiter from releasing a newer retry of the same logical job.
	expected := RunningJobState{RunID: rj.Data.RunID, StartedAt: startTime}
	r.beginPostJobFinalization(runDir)
	defer r.endPostJobFinalization(runDir)
	released := r.state.FinishRunningAttempt(jobIDStr, expected, ei.ExitCode, endTime)
	if released {
		oplog.LogJob("job.slot_released", jobID, "", oplog.WithDetailf("run_id=%d exit=%d", rj.Data.RunID, ei.ExitCode))
		r.saveState()
	}

	// The process has been observed through Wait, so local supervisor cleanup
	// no longer owns capacity. Delete only this exact process pointer: a newer
	// retry may already have installed its own supervisor under the same job ID.
	r.processesMu.Lock()
	if r.processes[jobIDStr] == proc {
		delete(r.processes, jobIDStr)
	}
	r.processesMu.Unlock()
	CleanupPIDFiles(paths)
	removeJobFile(r.queueDir, jobID)
	removePerJobSourceMarker(rj.Data.Dir, jobID)

	// Stop live log uploader, then hand durable publication work off. These
	// steps are intentionally downstream of slot release.
	if stopFn != nil {
		stopFn()
	}
	cleanupDir := ""
	if usesIsolatedSource(rj.Data) && r.PostJobManager != nil {
		cleanupDir = filepath.Dir(perJobSourceDir(jobID))
	}
	if r.PostJobManager != nil {
		r.PostJobManager.StartPostJob(PostJobCapture{
			JobID:      jobID,
			RunID:      rj.Data.RunID,
			WorkDir:    runDir,
			LogDir:     filepath.Dir(paths.Log),
			ExitCode:   ei.ExitCode,
			StartTime:  startTime,
			OutputDirs: rj.Data.OutputDirs,
			CleanupDir: cleanupDir,
		})
	}
	if r.OnJobFinish != nil {
		r.OnJobFinish(jobID, rj.Data.RunID, filepath.Dir(paths.Log), ei.ExitCode)
	}
	// In R2-isolated mode the runtime source lives under a per-job dir we
	// own; remove it now so ~/.cache/weft/jobs/ doesn't grow unbounded.
	if usesIsolatedSource(rj.Data) && cleanupDir == "" {
		removePerJobSourceDir(jobID)
	}

	r.saveState()
}

func (r *Runner) beginPostJobFinalization(workdir string) {
	workdir = ExpandTilde(workdir)
	r.processesMu.Lock()
	r.finalizingWorkdirs[workdir]++
	r.processesMu.Unlock()
}

func (r *Runner) endPostJobFinalization(workdir string) {
	workdir = ExpandTilde(workdir)
	r.processesMu.Lock()
	if r.finalizingWorkdirs[workdir] <= 1 {
		delete(r.finalizingWorkdirs, workdir)
	} else {
		r.finalizingWorkdirs[workdir]--
	}
	r.processesMu.Unlock()
}

func (r *Runner) postJobFinalizationPending(job *opsqueue.CommandJob) bool {
	r.processesMu.Lock()
	defer r.processesMu.Unlock()
	if HasBenchmarkTag(&RunnerJob{Data: job}) {
		return len(r.finalizingWorkdirs) > 0
	}
	return r.finalizingWorkdirs[ExpandTilde(job.Dir)] > 0
}

// perJobSourceDir returns the per-job working directory used in R2-isolated
// mode (CommandJob.SourceR2Key). Tarballs are extracted here so jobs running
// off the same project don't share filesystem state.
func perJobSourceDir(jobID int64) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".cache", "weft", "jobs", fmt.Sprintf("%d", jobID), "source")
}

func usesIsolatedSource(job *opsqueue.CommandJob) bool {
	return job != nil && (job.SourceManifest != nil || job.SourceR2Key != "")
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
		rs, ok := r.state.GetRunning(jobIDStr)
		if !ok {
			continue
		}

		// A matching completion record is authoritative terminal evidence. It
		// releases occupancy even if a stale in-memory waiter remains after a
		// callback blocked, or after restart lost the original observation.
		if rec, err := ReadCompletionRecord(paths); err == nil && completionMatchesRunningAttempt(rec, rs) {
			if r.state.FinishRunningAttempt(jobIDStr, rs, rec.ExitCode, rec.EndTime) {
				oplog.LogJob("job.slot_released", jobID, "", oplog.WithDetailf("run_id=%d exit=%d recovered=true", rec.RunID, rec.ExitCode))
				CleanupPIDFiles(paths)
				changed = true
				if r.OnJobFinish != nil {
					r.OnJobFinish(jobID, rs.RunID, filepath.Dir(paths.Log), rec.ExitCode)
				}
			}
			continue
		} else if err != nil && !os.IsNotExist(err) {
			slog.Warn("completion record unreadable; terminality unknown", "component", "runner", "job_id", jobID, "error", err)
		}

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
				endTime := r.now().Unix()
				if exitCode == 0 {
					oplog.LogJob(oplog.OpJobComplete, jobID, "", oplog.WithDetail("exit=0 (recovered)"))
					slog.Info("job completed (recovered)", "component", "runner", "job_id", jobID)
				} else {
					failureReason := DetectFailureReasonFromExitInfo(ei)
					WriteFailureReasonFile(paths, failureReason)
					oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetailf("exit=%d (recovered)", exitCode))
					slog.Warn("job failed (recovered)", "component", "runner", "job_id", jobID, "exit_code", exitCode)
				}
				outputFiles := r.discoverRecoveredOutputs(jobID, exitCode, rs, paths)
				WriteCompletionRecord(paths, ei, rs, "", "", rs.StartedAt, endTime, outputFiles)
				WriteRusageFile(paths, rs)
				r.state.FinishRunningAttempt(jobIDStr, rs, exitCode, endTime)
				CleanupPIDFiles(paths)
				changed = true
				if r.OnJobFinish != nil {
					r.OnJobFinish(jobID, rs.RunID, filepath.Dir(paths.Log), exitCode)
				}
			}
			continue
		}

		// Check if process is stopped (state T) without .paused marker
		pgid, hasPGID, pgidErr := ReadPIDFileDetailed(paths.PGID)
		pid, hasPID, pidErr := ReadPIDFileDetailed(paths.PID)
		if pgidErr != nil || pidErr != nil {
			oplog.LogJob("job.pidfile_unknown", jobID, "",
				oplog.WithDetailf("pid_err=%v pgid_err=%v", pidErr, pgidErr))
			continue
		}

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
			WriteRusageFile(paths, rs)
			endTime := r.now().Unix()
			WriteCompletionRecord(paths, stoppedEI, rs, KillReasonStoppedDetected, FailureReasonError, rs.StartedAt, endTime, nil)
			r.state.FinishRunningAttempt(jobIDStr, rs, 1, endTime)
			CleanupPIDFiles(paths)
			changed = true
			if r.OnJobFinish != nil {
				r.OnJobFinish(jobID, rs.RunID, filepath.Dir(paths.Log), 1)
			}
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
			endTime := r.now().Unix()
			if exitCode == 0 {
				oplog.LogJob(oplog.OpJobComplete, jobID, "", oplog.WithDetail("exit=0 (recovered after orphan check)"))
			} else {
				failureReason := DetectFailureReasonFromExitInfo(ei)
				WriteFailureReasonFile(paths, failureReason)
				oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetailf("exit=%d (recovered after orphan check)", exitCode))
			}
			outputFiles := r.discoverRecoveredOutputs(jobID, exitCode, rs, paths)
			WriteCompletionRecord(paths, ei, rs, "", "", rs.StartedAt, endTime, outputFiles)
			WriteRusageFile(paths, rs)
			r.state.FinishRunningAttempt(jobIDStr, rs, exitCode, endTime)
			CleanupPIDFiles(paths)
			changed = true
			if r.OnJobFinish != nil {
				r.OnJobFinish(jobID, rs.RunID, filepath.Dir(paths.Log), exitCode)
			}
			continue
		}

		// No status file — truly orphaned, exit code unknown
		orphanEI := ExitInfo{ExitCode: 1}
		WriteStatusFile(paths, orphanEI)
		oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetail("exit=1 duration=0"))
		endTime := r.now().Unix()
		WriteCompletionRecord(paths, orphanEI, rs, KillReasonOrphan, FailureReasonError, rs.StartedAt, endTime, nil)
		WriteRusageFile(paths, rs)
		r.state.FinishRunningAttempt(jobIDStr, rs, 1, endTime)
		CleanupPIDFiles(paths)
		changed = true
		if r.OnJobFinish != nil {
			r.OnJobFinish(jobID, rs.RunID, filepath.Dir(paths.Log), 1)
		}
	}

	if changed {
		r.saveState()
	}
}

func completionMatchesRunningAttempt(rec CompletionRecord, running RunningJobState) bool {
	if running.RunID != 0 || rec.RunID != 0 {
		return running.RunID != 0 && running.RunID == rec.RunID
	}
	return running.StartedAt != 0 && running.StartedAt == rec.StartTime
}

// discoverRecoveredOutputs discovers convention-based output files for a job
// whose completion was recovered outside waitForJob (runner restart, wrapper
// exit trap). It mirrors waitForJob's success-path discovery so the
// completion record carries output_files, which `weft artifact sync` uses to
// fetch results without guessing.
//
// Recovery can run long after the process actually exited, so the discovery
// window is bounded on both sides: at attempt start − 1s (spec:
// OutputDiscoveryUsesAttemptStartCutoff in specs/job-lifecycle.allium) and at
// the status-file mtime + artifacts.AttemptOutputEndSlack (the wrapper writes
// the status file at process exit), so files written later by sibling jobs
// sharing the output tree are not attributed to this job. A job with no
// recorded start has no attribution window and gets no discovery — matching
// everything would claim other jobs' files.
func (r *Runner) discoverRecoveredOutputs(jobID int64, exitCode int, rs RunningJobState, paths JobPaths) []OutputFile {
	if exitCode != 0 || rs.StartedAt == 0 {
		return nil
	}
	workDir := rs.DiskPath
	var dirs, refs []string
	if rj, err := ReadJobFile(r.queueDir, jobID); err == nil {
		dirs = rj.OutputDirs
		refs = rj.Outputs
		if workDir == "" {
			workDir = rj.Dir
		}
	}
	if workDir == "" {
		return nil
	}
	if len(dirs) == 0 {
		dirs = config.DefaultOutputDirs
	}
	lower := AttemptOutputThreshold(rs.StartedAt)
	discovered, err := DiscoverJobOutputsSince(workDir, dirs, refs, lower)
	if err != nil || len(discovered) == 0 {
		return nil
	}
	if info, statErr := os.Stat(paths.Status); statErr == nil {
		discovered = FilterOutputFilesUntil(workDir, discovered, info.ModTime().Add(artifacts.AttemptOutputEndSlack))
	}
	if len(discovered) > 0 {
		oplog.LogJob("job.outputs_discovered", jobID, "", oplog.WithDetailf("files=%d total_mb=%d (recovered)", len(discovered), TotalSizeMB(discovered)))
	}
	return discovered
}

func (r *Runner) sampleRunningJobs() {
	now := r.now()
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

		if r.state.UpdateRunningAttempt(jobIDStr, rs, rs) {
			updated = true
		}
	}

	if updated {
		r.saveState()
	}
}

func (r *Runner) adjustRunningJobAllotments() {
	now := r.now()
	updated := false

	for _, jobIDStr := range r.state.RunningIDs() {
		jobID := mustParseInt64(jobIDStr)
		rs, ok := r.state.GetRunning(jobIDStr)
		if !ok || rs.WarmupUntil > now.Unix() {
			continue
		}

		hostPct, ok := r.observeJobCPU(jobID, r.cpuCount)
		if !ok {
			continue
		}

		oldAllotment := rs.LocalAllotment
		next, changed := r.cpuConfig.ApplyCPUObservation(CPUAllotmentState{
			WarmupUntil:    rs.WarmupUntil,
			LocalAllotment: rs.LocalAllotment,
			Samples:        rs.Samples,
			OverHist:       rs.OverHist,
			UnderHist:      rs.UnderHist,
		}, now.Unix(), hostPct, r.state.TotalAllotment())
		if changed {
			if next.LocalAllotment > oldAllotment {
				oplog.LogJob("job.allotment_increase", jobID, "", oplog.WithDetailf("cpu=%d", next.LocalAllotment))
			} else {
				oplog.LogJob("job.allotment_decay", jobID, "", oplog.WithDetailf("cpu=%d", next.LocalAllotment))
			}
		}
		rs.LocalAllotment = next.LocalAllotment
		rs.Samples = next.Samples
		rs.OverHist = next.OverHist
		rs.UnderHist = next.UnderHist

		if r.state.UpdateRunningAttempt(jobIDStr, rs, rs) {
			updated = true
		}
	}

	if updated {
		r.saveState()
	}
}

func (r *Runner) observeRunningJobCPU(jobID int64, cpuCount int) (int, bool) {
	paths := NewJobPaths(r.logDir, jobID)
	pid, ok := ReadPIDFile(paths.PID)
	if !ok || !CheckPIDAlive(pid) {
		return 0, false
	}
	return ProcCPUHostPct(pid, cpuCount), true
}

// saveState saves the runner state to disk, logging any error.
// All callers use this instead of r.state.Save directly so that
// write failures (e.g. NFS unavailability) are never silently swallowed.
func (r *Runner) saveState() {
	if err := r.state.saveAt(r.stateFile, r.now()); err != nil {
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
	now := r.now().Unix()
	for _, rs := range r.state.RunningSnapshot() {
		if rs.WarmupUntil > now {
			return true
		}
	}
	return false
}

func (r *Runner) jobAllotment(job *opsqueue.CommandJob) int {
	// Explicit CPU percent wins: it is already destination-relative.
	if job.CPU != nil && *job.CPU > 0 {
		return *job.CPU
	}
	// An explicit core reservation is absolute, so normalize it against
	// this host's core count.
	if job.CPUReserveCores > 0 {
		return ReserveCoresAllotment(job.CPUReserveCores, r.cpuCount)
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
