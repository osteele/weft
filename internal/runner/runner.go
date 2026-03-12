package runner

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/ops"
)

// Runner is the main queue runner that manages job execution.
type Runner struct {
	queueName string
	queueDir  string
	logDir    string

	state     *State
	cmdProc   *CommandProcessor
	gpuInv    *GPUInventory
	cpuConfig CPUConfig
	benchCfg  BenchmarkConfig
	cpuCount  int

	// File paths
	commandsFile string
	stateFile    string
	currentFile  string
	pidFile      string
	runnerLog    string

	// Runtime state
	processes      map[string]*Process // jobID -> process
	processesMu    sync.Mutex
	lastSampleTime time.Time

	// Benchmark tracking
	benchmarkIdleCount  int
	benchmarkLastReason string

	// Shutdown
	stopCh chan struct{}
}

// Config holds configuration for the runner.
type Config struct {
	QueueName string
	QueueDir  string
	LogDir    string
}

// DefaultConfig returns a configuration with standard paths.
func DefaultConfig(queueName string) Config {
	home, _ := os.UserHomeDir()
	return Config{
		QueueName: queueName,
		QueueDir:  filepath.Join(home, ".cache", "weft", "queue"),
		LogDir:    filepath.Join(home, ".cache", "weft", "logs"),
	}
}

// New creates a new Runner with the given configuration.
func New(cfg Config) *Runner {
	return &Runner{
		queueName:    cfg.QueueName,
		queueDir:     cfg.QueueDir,
		logDir:       cfg.LogDir,
		commandsFile: filepath.Join(cfg.QueueDir, cfg.QueueName+".commands"),
		stateFile:    filepath.Join(cfg.QueueDir, cfg.QueueName+".state.json"),
		currentFile:  filepath.Join(cfg.QueueDir, cfg.QueueName+".current"),
		pidFile:      filepath.Join(cfg.QueueDir, cfg.QueueName+".runner.pid"),
		runnerLog:    filepath.Join(cfg.QueueDir, "runner-"+cfg.QueueName+".log"),
		cpuConfig:    DefaultCPUConfig(),
		benchCfg:     DefaultBenchmarkConfig(),
		processes:    make(map[string]*Process),
		stopCh:       make(chan struct{}),
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
	r.cmdProc = NewCommandProcessor(r.commandsFile, r.queueDir)

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
			WriteKillReasonFile(paths, "runner_shutdown")
		}
		close(r.stopCh)
	}()

	// Cleanup on exit
	defer func() {
		os.Remove(r.pidFile)
		os.Remove(r.currentFile)
	}()

	oplog.Log(oplog.OpQueueStart, oplog.WithDetailf("queue=%s pid=%d", r.queueName, os.Getpid()))
	fmt.Printf("Queue runner started for queue: %s\n", r.queueName)
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
		r.state.Save(r.stateFile)
		// Re-exec ourselves
		oplog.Log("cmd.restart")
		exe, _ := os.Executable()
		syscall.Exec(exe, os.Args, os.Environ())
	}

	// Refresh running jobs (check for completion)
	r.refreshRunningJobs()

	// Save state after processing
	r.state.Save(r.stateFile)

	// Check stop condition
	if r.state.StopRequested && r.state.PendingEmpty() && r.state.RunningCount() == 0 {
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
		return
	}

	if r.state.StopRequested {
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
		fmt.Fprintf(os.Stderr, "Job %d: cannot read job file: %v\n", jobID, err)
		return
	}
	rj := &RunnerJob{Data: job, ID: jobID}

	// Check exclusive constraints
	if AnyRunningExclusive(r.state, r.queueDir) {
		r.state.AddPending(jobID)
		r.state.Save(r.stateFile)
		return
	}

	runningCount := r.state.RunningCount()
	if HasExclusiveOrBenchmarkTag(rj) && runningCount > 0 {
		r.state.AddPending(jobID)
		r.state.Save(r.stateFile)
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
			r.state.AddPending(jobID)
			r.state.Save(r.stateFile)
			return
		}
		r.benchmarkIdleCount++
		if r.benchmarkIdleCount < r.benchCfg.IdleSamples {
			r.state.AddPending(jobID)
			r.state.Save(r.stateFile)
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
			r.state.AddPending(jobID)
			r.state.Save(r.stateFile)
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
		canStart, resolvedGPUDevices = r.gpuInv.CanStartGPUJob(r.state, rj)
		if !canStart {
			r.state.AddPending(jobID)
			r.state.Save(r.stateFile)
			return
		}
	}

	// Start the job
	log.Printf("Job %d: starting (runningCount=%d, gpuClass=%q, resolvedGPU=%v)",
		jobID, runningCount, job.GPUClass, resolvedGPUDevices)
	err = r.startJob(jobID, job, resolvedGPUDevices)
	if err == errRequeue {
		log.Printf("Job %d: requeued", jobID)
		r.state.AddPending(jobID)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "Job %d: start failed: %v\n", jobID, err)
	} else {
		log.Printf("Job %d: started successfully", jobID)
	}
	r.state.Save(r.stateFile)
}

var errRequeue = fmt.Errorf("requeue")

func (r *Runner) startJob(jobID int64, job *ops.CommandJob, preResolvedGPUDevices []string) error {
	jobIDStr := strconv.FormatInt(jobID, 10)
	command := job.Cmd
	if command == "" {
		fmt.Printf("Job %d: no command found, skipping\n", jobID)
		return nil
	}

	// Check if already completed or running
	if JobCompleted(r.logDir, jobID) {
		fmt.Printf("Job %d: already completed, skipping\n", jobID)
		removeJobFile(r.queueDir, jobID)
		return nil
	}

	// Check dependencies
	depResult := CheckDependencies(job.Deps, job.Needs, r.logDir)
	switch depResult.Result {
	case DepWaiting:
		fmt.Printf("Job %d: waiting for dependencies\n", jobID)
		return errRequeue
	case DepFailed:
		oplog.LogJob("job.skipped", jobID, "", oplog.WithDetailf("dependency %s failed", depResult.FailedDep))
		fmt.Printf("Job %d: skipped, dependency %s failed\n", jobID, depResult.FailedDep)
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

	startTime := time.Now().Unix()
	paths := NewJobPaths(r.logDir, jobID)

	// Archive existing files only when the job is definitely launching.
	ArchiveExistingFiles(r.logDir, jobID)

	oplog.LogJob(oplog.OpJobStart, jobID, "", oplog.WithDetailf("cmd=%s", command))
	fmt.Printf("==========================================\n")
	fmt.Printf("Starting job %d\n", jobID)
	fmt.Printf("  Working dir: %s\n", job.Dir)
	fmt.Printf("  Command: %s\n", command)
	if job.Desc != "" {
		fmt.Printf("  Description: %s\n", job.Desc)
	}
	fmt.Printf("  Log: %s\n", paths.Log)
	fmt.Printf("==========================================\n")

	// Write metadata
	WriteMetaFile(paths, jobID, job.Dir, command, job.Desc, r.queueName, startTime)

	// Write log header
	WriteLogHeader(paths, jobID, job.Dir, command)

	// Write gpu_devices to meta file so the coordinator can discover them
	if len(gpuDevices) > 0 {
		if f, err := os.OpenFile(paths.Meta, os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			fmt.Fprintf(f, "gpu_devices=%s\n", strings.Join(gpuDevices, ","))
			f.Close()
		}
	}

	// Build environment
	var envVars []string

	// Expand ~ in working directory
	expandedDir := job.Dir
	if len(expandedDir) > 1 && expandedDir[0] == '~' {
		home, _ := os.UserHomeDir()
		expandedDir = home + expandedDir[1:]
	}

	// Load dotenv files from working directory
	if expandedDir != "" {
		dotenvVars, _ := LoadDotenvFiles(expandedDir)
		envVars = append(envVars, dotenvVars...)
	}

	// Apply job env vars (override dotenv)
	envVars = append(envVars, job.Env...)

	// Inject resolved GPU device
	if len(gpuDevices) > 0 && job.GPUClass != "" {
		cudaEnv := FormatGPUDeviceEnv(gpuDevices)
		envVars = append(envVars, cudaEnv)
	}

	// Run environment setup as a separate phase
	if setupCmd := DetectSetupCommand(expandedDir); setupCmd != "" {
		ei, setupErr := RunSetupCommand(setupCmd, jobID, job.Dir, envVars, paths)
		if setupErr != nil {
			oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetailf("setup failed exit=%d", ei.ExitCode))
			return setupErr
		}
		if setupCmd == "uv sync" {
			collectAndWriteUVManifest(jobID, expandedDir, filepath.Dir(paths.Log))
		}
	}

	// Start the process
	log.Printf("Job %d: launching process", jobID)
	proc, err := StartProcess(command, job.Dir, envVars, paths.Log)
	if err != nil {
		oplog.LogJob(oplog.OpJobStartFailed, jobID, "", oplog.WithError(err))
		log.Printf("Job %d: start failed: %v", jobID, err)
		os.WriteFile(paths.Status, []byte("1\n"), 0644)
		return fmt.Errorf("start process: %w", err)
	}

	// Write PID/PGID files
	log.Printf("Job %d: started PID=%d, writing PID files", jobID, proc.PID)
	proc.WritePIDFiles(paths)

	// Track the process
	r.processesMu.Lock()
	r.processes[jobIDStr] = proc
	r.processesMu.Unlock()

	// Start wait goroutine
	go r.waitForJob(jobID, proc, paths, startTime, rj)

	// Update running state
	allotment := r.jobAllotment(job)
	gpuMemGB := GetJobGPUMem(job, DefaultGPUMemGB)

	r.state.AddRunning(jobIDStr, RunningJobState{
		StartedAt:      startTime,
		WarmupUntil:    startTime + int64(r.cpuConfig.WarmupDuration),
		LocalAllotment: allotment,
		Samples:        []int{},
		GPUDevices:     gpuDevices,
		GPUMemGB:       gpuMemGB,
	})

	return nil
}

func (r *Runner) waitForJob(jobID int64, proc *Process, paths JobPaths, startTime int64, rj *RunnerJob) {
	jobIDStr := strconv.FormatInt(jobID, 10)
	err := proc.Cmd.Wait()
	ei := ExtractExitInfo(err)
	log.Printf("Job %d: process exited (code=%d, signal=%v, err=%v)", jobID, ei.ExitCode, ei.Signaled, err)

	endTime := time.Now().Unix()
	duration := endTime - startTime

	// Write status and log footer
	WriteStatusFile(paths, ei)
	WriteLogFooter(paths, ei)

	// Write artifact satisfied files for producer jobs
	for _, spec := range rj.Data.Produces {
		parsed := ParseProducesSpec(spec)
		version := parsed.Version
		if version == 0 {
			version = jobID
		}
		satisfiedPath := ArtifactSatisfiedFile(r.logDir, parsed.Path, version)
		if err := os.WriteFile(satisfiedPath, []byte(fmt.Sprintf("%d\n", ei.ExitCode)), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "Job %d: failed to write artifact satisfied file %s: %v\n", jobID, satisfiedPath, err)
		}
	}

	// On failure, detect the failure reason using signal-aware detection
	var failureReason string
	if ei.ExitCode != 0 {
		failureReason = DetectFailureReasonFromExitInfo(ei)
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
		if discovered, err := DiscoverOutputs(rj.Data.Dir, dirs); err == nil && len(discovered) > 0 {
			outputFiles = discovered
			oplog.LogJob("job.outputs_discovered", jobID, "", oplog.WithDetailf("files=%d total_mb=%d", len(discovered), TotalSizeMB(discovered)))
			fmt.Printf("Job %d: discovered %d output files\n", jobID, len(discovered))
		}
		oplog.LogJob(oplog.OpJobComplete, jobID, "", oplog.WithDetailf("exit=0 duration=%ds", duration))
		fmt.Printf("Job %d completed successfully\n", jobID)
	} else {
		reason := ReadFailureReasonFile(paths.FailureReason)
		detail := fmt.Sprintf("exit=%d duration=%ds reason=%s", ei.ExitCode, duration, reason)
		if ei.Signaled {
			detail += fmt.Sprintf(" signal=%s", ei.SignalName())
		}
		oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetail(detail))
		fmt.Printf("Job %d failed with exit code %d (%s)\n", jobID, ei.ExitCode, reason)
	}

	// Write rusage and completion record
	r.processesMu.Lock()
	rs := r.state.Running[jobIDStr]
	r.processesMu.Unlock()
	WriteRusageFile(paths, rs)
	killReason := ReadKillReasonFile(paths.KillReason)
	WriteCompletionRecord(paths, ei, rs, killReason, failureReason, startTime, endTime, outputFiles)

	// Record finished and remove from running
	r.state.RecordFinished(jobIDStr, ei.ExitCode, endTime)
	r.state.RemoveRunning(jobIDStr)

	// Cleanup
	r.processesMu.Lock()
	delete(r.processes, jobIDStr)
	r.processesMu.Unlock()

	CleanupPIDFiles(paths)
	removeJobFile(r.queueDir, jobID)

	r.state.Save(r.stateFile)
}

func (r *Runner) refreshRunningJobs() {
	changed := false
	for _, jobIDStr := range r.state.RunningIDs() {
		jobID := mustParseInt64(jobIDStr)
		paths := NewJobPaths(r.logDir, jobID)

		// Check if status file appeared (job completed outside our wait goroutine)
		if _, err := os.Stat(paths.Status); err == nil {
			// Job completed — the wait goroutine should handle this,
			// but check if it hasn't yet (e.g., bash runner state recovery)
			r.processesMu.Lock()
			_, hasProc := r.processes[jobIDStr]
			r.processesMu.Unlock()

			if !hasProc {
				// No active wait goroutine — handle completion here
				exitCode, _ := ReadStatusFile(paths.Status)
				rs := r.state.Running[jobIDStr]
				WriteRusageFile(paths, rs)
				finishedAt := time.Now().Unix()
				r.state.RecordFinished(jobIDStr, exitCode, finishedAt)
				r.state.RemoveRunning(jobIDStr)
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
			fmt.Printf("Job %d process %d is stopped (state T) - marking as failed\n", jobID, checkPID)
			oplog.LogJob("job.stopped_detected", jobID, "", oplog.WithDetailf("pid=%d state=T", checkPID))

			WriteKillReasonFile(paths, "stopped_detected")

			if hasPGID {
				KillProcessGroup(pgid)
			}
			if hasPID && pid != pgid {
				syscall.Kill(pid, syscall.SIGKILL)
			}

			stoppedEI := ExitInfo{ExitCode: 1}
			WriteStatusFile(paths, stoppedEI)
			oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetail("exit=1 reason=stopped"))
			rs := r.state.Running[jobIDStr]
			WriteRusageFile(paths, rs)
			endTime := time.Now().Unix()
			WriteCompletionRecord(paths, stoppedEI, rs, "stopped_detected", "stopped", rs.StartedAt, endTime, nil)
			r.state.RecordFinished(jobIDStr, 1, endTime)
			r.state.RemoveRunning(jobIDStr)
			CleanupPIDFiles(paths)
			changed = true
			continue
		}

		// Check if wrapper process is still alive
		if hasPID && CheckPIDAlive(pid) {
			continue
		}

		// Wrapper gone — kill orphaned process group
		if hasPGID && CheckPIDAlive(pgid) {
			WriteKillReasonFile(paths, "orphan")
			fmt.Printf("Killing orphaned process group %d for job %d\n", pgid, jobID)
			KillProcessGroup(pgid)
			oplog.LogJob("job.orphan_killed", jobID, "", oplog.WithDetailf("pgid=%d", pgid))
		}

		// Mark as failed
		if _, err := os.Stat(paths.Status); err != nil {
			orphanEI := ExitInfo{ExitCode: 1}
			WriteStatusFile(paths, orphanEI)
			oplog.LogJob(oplog.OpJobFail, jobID, "", oplog.WithDetail("exit=1 duration=0"))
			endTime := time.Now().Unix()
			rs := r.state.Running[jobIDStr]
			WriteCompletionRecord(paths, orphanEI, rs, "orphan", "orphan", rs.StartedAt, endTime, nil)
			r.state.RecordFinished(jobIDStr, 1, endTime)
		}
		rs := r.state.Running[jobIDStr]
		WriteRusageFile(paths, rs)
		r.state.RemoveRunning(jobIDStr)
		CleanupPIDFiles(paths)
		changed = true
	}

	if changed {
		r.state.Save(r.stateFile)
	}
}

func (r *Runner) sampleRunningJobs() {
	now := time.Now()
	updated := false

	for _, jobIDStr := range r.state.RunningIDs() {
		jobID := mustParseInt64(jobIDStr)
		paths := NewJobPaths(r.logDir, jobID)

		rs, ok := r.state.Running[jobIDStr]
		if !ok {
			continue
		}

		// Skip during warmup
		if rs.WarmupUntil > now.Unix() {
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
		pressure, hostPct := SampleJob(pid, pgid, r.cpuCount, paths, &rs, "multi")

		// Log pressure escalation
		if MemPressureSeverity(pressure) > MemPressureSeverity(oldPressure) {
			if oldPressure != "" && oldPressure != MemPressureNormal {
				oplog.LogJob("job.mem_pressure", jobID, "", oplog.WithDetailf("level=%s", pressure))
			}
		}

		// CPU sample history for allotment hysteresis
		rs.Samples = appendBounded(rs.Samples, hostPct, r.cpuConfig.SampleCount())

		// CPU allotment hysteresis
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

		r.state.Running[jobIDStr] = rs
		updated = true
	}

	if updated {
		r.state.Save(r.stateFile)
	}
}

func (r *Runner) warmupActive() bool {
	now := time.Now().Unix()
	for _, rs := range r.state.Running {
		if rs.WarmupUntil > now {
			return true
		}
	}
	return false
}

func (r *Runner) jobAllotment(job *ops.CommandJob) int {
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
	if r.state.Current == nil {
		os.Remove(r.currentFile)
	} else {
		os.WriteFile(r.currentFile, []byte(fmt.Sprintf("%d\n", *r.state.Current)), 0644)
	}
}
