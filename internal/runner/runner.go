package runner

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

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
	jobIDStr := strconv.FormatInt(jobID, 10)

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

	// Check GPU capacity
	if runningCount > 0 {
		canStart, _ := r.gpuInv.CanStartGPUJob(r.state, rj)
		if !canStart {
			r.state.AddPending(jobID)
			r.state.Save(r.stateFile)
			return
		}
	}

	// Start the job
	err = r.startJob(jobID, jobIDStr, job)
	if err == errRequeue {
		r.state.AddPending(jobID)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "Job %d: start failed: %v\n", jobID, err)
	}
	r.state.Save(r.stateFile)
}

var errRequeue = fmt.Errorf("requeue")

func (r *Runner) startJob(jobID int64, jobIDStr string, job *ops.CommandJob) error {
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
	depResult := CheckDependencies(job.Deps, r.logDir)
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

	startTime := time.Now().Unix()
	paths := NewJobPaths(r.logDir, jobID)

	// Archive existing files
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

	// Resolve GPU devices
	rj := &RunnerJob{Data: job, ID: jobID}
	var gpuDevices []string

	if job.GPUClass != "" {
		memPerDevice := GetJobGPUMem(job, DefaultGPUMemGB)
		device, ok := r.gpuInv.PickBestGPUForClass(r.state, job.GPUClass, memPerDevice)
		if !ok {
			fmt.Printf("Job %d: no available GPU device for class '%s', re-queuing\n", jobID, job.GPUClass)
			return errRequeue
		}
		gpuDevices = []string{device}
		fmt.Printf("  GPU class '%s' resolved to device %s\n", job.GPUClass, device)
	} else {
		gpuDevices = GetJobGPUDevices(job)
	}

	// Build environment
	var envVars []string

	// Load dotenv files from working directory
	if job.Dir != "" {
		dir := job.Dir
		if len(dir) > 1 && dir[0] == '~' {
			home, _ := os.UserHomeDir()
			dir = home + dir[1:]
		}
		dotenvVars, _ := LoadDotenvFiles(dir)
		envVars = append(envVars, dotenvVars...)
	}

	// Apply job env vars (override dotenv)
	envVars = append(envVars, job.Env...)

	// Inject resolved GPU device
	if len(gpuDevices) > 0 && job.GPUClass != "" {
		envVars = append(envVars, FormatGPUDeviceEnv(gpuDevices))
	}

	// Start the process
	proc, err := StartProcess(command, job.Dir, envVars, paths.Log)
	if err != nil {
		oplog.LogJob(oplog.OpJobStartFailed, jobID, "", oplog.WithError(err))
		os.WriteFile(paths.Status, []byte("1\n"), 0644)
		return fmt.Errorf("start process: %w", err)
	}

	// Write PID/PGID files
	proc.WritePIDFiles(paths)

	// Track the process
	r.processesMu.Lock()
	r.processes[jobIDStr] = proc
	r.processesMu.Unlock()

	// Start wait goroutine
	go r.waitForJob(jobID, jobIDStr, proc, paths, startTime, rj)

	// Update running state
	allotment := r.jobAllotment(job)
	gpuMemGB := GetJobGPUMem(job, DefaultGPUMemGB)
	_ = rj

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

func (r *Runner) waitForJob(jobID int64, jobIDStr string, proc *Process, paths JobPaths, startTime int64, rj *RunnerJob) {
	err := proc.Cmd.Wait()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	endTime := time.Now().Unix()
	duration := endTime - startTime

	// Write status and log footer
	WriteStatusFile(paths, exitCode)
	WriteLogFooter(paths, exitCode)

	// On failure, detect the failure reason (OOM, GPU OOM, etc.) and write it
	if exitCode != 0 {
		reason := DetectFailureReason(exitCode)
		WriteFailureReasonFile(paths, reason)
	}

	// Append end_time to meta file
	if f, err := os.OpenFile(paths.Meta, os.O_APPEND|os.O_WRONLY, 0644); err == nil {
		fmt.Fprintf(f, "end_time=%d\n", endTime)
		f.Close()
	}

	if exitCode == 0 {
		oplog.LogJob(oplog.OpJobCompleted, jobID, "", oplog.WithDetailf("exit=0 duration=%ds", duration))
		fmt.Printf("Job %d completed successfully\n", jobID)
	} else {
		reason := ReadFailureReasonFile(paths.FailureReason)
		oplog.LogJob(oplog.OpJobFailed, jobID, "", oplog.WithDetailf("exit=%d duration=%ds reason=%s", exitCode, duration, reason))
		fmt.Printf("Job %d failed with exit code %d (%s)\n", jobID, exitCode, reason)
	}

	// Write rusage
	r.processesMu.Lock()
	if rs, ok := r.state.Running[jobIDStr]; ok {
		WriteRusageFile(paths, rs)
	}
	r.processesMu.Unlock()

	// Record finished and remove from running
	r.state.RecordFinished(jobIDStr, exitCode, endTime)
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

			if hasPGID {
				KillProcessGroup(pgid)
			}
			if hasPID && pid != pgid {
				syscall.Kill(pid, syscall.SIGKILL)
			}

			os.WriteFile(paths.Status, []byte("1\n"), 0644)
			oplog.LogJob(oplog.OpJobFailed, jobID, "", oplog.WithDetail("exit=1 reason=stopped"))
			rs := r.state.Running[jobIDStr]
			WriteRusageFile(paths, rs)
			r.state.RecordFinished(jobIDStr, 1, time.Now().Unix())
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
			fmt.Printf("Killing orphaned process group %d for job %d\n", pgid, jobID)
			KillProcessGroup(pgid)
			oplog.LogJob("job.orphan_killed", jobID, "", oplog.WithDetailf("pgid=%d", pgid))
		}

		// Mark as failed
		if _, err := os.Stat(paths.Status); err != nil {
			os.WriteFile(paths.Status, []byte("1\n"), 0644)
			oplog.LogJob(oplog.OpJobFailed, jobID, "", oplog.WithDetail("exit=1 duration=0"))
			r.state.RecordFinished(jobIDStr, 1, time.Now().Unix())
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

		// CPU sample
		hostPct := ProcCPUHostPct(pid, r.cpuCount)
		WriteSample(paths, now.Unix(), hostPct, nil)

		rs.Samples = appendBounded(rs.Samples, hostPct, r.cpuConfig.SampleCount())

		// Resource usage from /proc
		if _, err := os.Stat("/proc"); err == nil {
			rusagePID := pid
			if pgid, ok := ReadPIDFile(paths.PGID); ok {
				rusagePID = pgid
			}
			userTicks, sysTicks, peakRSS := ProcResourceUsage(rusagePID)
			rs.RusageUserCPU = TicksToSeconds(userTicks)
			rs.RusageSysCPU = TicksToSeconds(sysTicks)

			peakRSSStr := strconv.FormatInt(peakRSS, 10)
			if rs.RusagePeakRSS == "" || peakRSS > mustParseInt64(rs.RusagePeakRSS) {
				rs.RusagePeakRSS = peakRSSStr
			}
		}

		// GPU memory sampling
		gpuMem := ProcGPUMemMiB(pid)
		if gpuMem > 0 {
			gpuMemStr := strconv.Itoa(gpuMem)
			if rs.RusageMaxGPU == "" {
				rs.RusageMaxGPU = gpuMemStr
			} else if prev, _ := strconv.Atoi(rs.RusageMaxGPU); gpuMem > prev {
				rs.RusageMaxGPU = gpuMemStr
			}
		}

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
