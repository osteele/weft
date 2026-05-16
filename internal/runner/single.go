package runner

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/opsqueue"
	"github.com/osteele/weft/internal/remediation"
)

// SingleJobConfig configures a single-shot job execution.
type SingleJobConfig struct {
	JobID           int64
	Job             opsqueue.CommandJob
	LogDir          string
	WorkingDir      string             // Override job.Dir if non-empty
	SampleInterval  time.Duration      // Default 1s
	MaxTime         time.Duration      // If >0, kill the job after this duration
	SetupTimeout    time.Duration      // If >0, kill the setup command after this duration (default 20m)
	SetupPrewarmed  bool               // If true, skip the detected setup command
	SetupPrewarmLog string             // Optional setup prewarm log to append to this job log
	SkipProbes      bool               // Skip cache size probes (useful in tests)
	OnPhase         func(phase string) // Called at phase transitions: "setup", "running"

	// Hang-detection watchdogs. Zero disables. See specs/job-lifecycle.allium
	// rules GPUIdleKillsJob and StdoutSilenceKillsJob.
	GPUIdleTimeout       time.Duration // Kill if all assigned GPUs report 0% for this long (after arming)
	StdoutSilenceTimeout time.Duration // Kill if log file doesn't grow for this long (after arming)
	WatchdogInitialGrace time.Duration // Grace window before watchdogs arm (default 5m)

	// Test hook: returns true when any assigned GPU has non-zero utilization.
	// If nil, watchdog polls nvidia-smi via HostGPUMetrics.
	GPUActiveProbe func() bool
}

// watchdogTickPeriod returns the check interval for a hang watchdog given its
// timeout. A fraction of the timeout, bounded to a sane range.
func watchdogTickPeriod(timeout time.Duration) time.Duration {
	p := timeout / 10
	if p < 50*time.Millisecond {
		return 50 * time.Millisecond
	}
	if p > 30*time.Second {
		return 30 * time.Second
	}
	return p
}

// RunSingleJob executes a single job synchronously with full telemetry.
// It handles GPU discovery, process management, sampling, failure detection,
// completion records, and output discovery — the same as the queue runner
// but without queue/scheduling logic.
func RunSingleJob(cfg SingleJobConfig) (ExitInfo, error) {
	if cfg.SampleInterval == 0 {
		cfg.SampleInterval = time.Second
	}
	telemetryPolicy := TelemetryPolicyForJob(&cfg.Job)
	if HasTag(&cfg.Job, "benchmark") && telemetryPolicy.Interval > cfg.SampleInterval {
		cfg.SampleInterval = telemetryPolicy.Interval
	}

	job := &cfg.Job
	command := job.Cmd
	if command == "" {
		return ExitInfo{ExitCode: 1}, fmt.Errorf("empty command")
	}

	workingDir := cfg.WorkingDir
	if workingDir == "" {
		workingDir = job.Dir
	}

	// Phase timing
	var phases PhaseTiming
	phases.WrapperStart = time.Now().Unix()

	// Discover hardware
	gpuInv := DiscoverGPUs()
	cpuCount := DetectCPUCount()

	// Setup phase
	if cfg.OnPhase != nil {
		cfg.OnPhase("setup")
	}
	phases.SetupStart = time.Now().Unix()

	os.MkdirAll(cfg.LogDir, 0755)

	paths := NewJobPaths(cfg.LogDir, cfg.JobID)

	// Archive existing files
	ArchiveExistingFiles(cfg.LogDir, cfg.JobID)

	// Expand ~ in working directory
	expandedDir := ExpandTilde(workingDir)

	// Write metadata
	WriteMetaFile(paths, cfg.JobID, workingDir, command, job.Desc, phases.WrapperStart, "")
	WriteLogHeader(paths, cfg.JobID, workingDir, command, "")
	if cfg.SetupPrewarmLog != "" {
		appendPrewarmLog(paths.Log, cfg.SetupPrewarmLog)
	}

	// Build environment
	var envVars []string
	if expandedDir != "" {
		dotenvVars, _ := LoadDotenvFiles(expandedDir)
		envVars = append(envVars, dotenvVars...)
	}

	setupCmd := DetectSetupCommand(expandedDir)
	if setupCmd == "uv sync" {
		scriptMeta, _ := dataloc.ScanScriptMeta(expandedDir, command)
		if ShouldSkipSetup(setupCmd, scriptMeta) {
			slog.Info("skipping uv sync: script metadata declares isolated = true",
				"component", "runner", "job_id", cfg.JobID)
			setupCmd = ""
		}
	}
	if setupCmd == direnvSetupCommand {
		resolvedEnv, ei, resolveErr := ResolveDirenvEnv(expandedDir, envVars, paths.Log)
		if resolveErr != nil {
			now := time.Now().Unix()
			phases.SetupEnd = now
			WriteStatusFile(paths, ei)

			failureReason := DetectFailureReasonFromExitInfoAndLog(ei, paths.Log)
			WriteFailureReasonFile(paths, failureReason)
			WriteCompletionRecord(paths, ei, RunningJobState{}, "", failureReason, phases.SetupStart, now, nil)
			WritePhasesFile(paths, phases)

			return ei, resolveErr
		}
		envVars = resolvedEnv
		setupCmd = ""
	}
	if setupCmd == "" {
		WarnIfWorkdirMissingEnv(expandedDir, cfg.JobID, paths.Log)
	}
	envVars = append(envVars, job.Env...)
	envVars = artifacts.MergeEnvVars(envVars, cfg.JobID)

	// Pre-register --produces declarations in the artifact manifest before
	// the job starts. The periodic output uploader (60s tick on cloud agents)
	// reads this manifest, so registering early lets large in-progress files
	// (e.g. checkpoints written every N steps) be uploaded throughout the
	// run rather than only after the script exits. This is idempotent with
	// the post-exit call below; the post-exit call covers paths the script
	// itself appended to $WEFT_ARTIFACT_MANIFEST during the run.
	if len(job.Produces) > 0 {
		if err := RecordProducedArtifacts(cfg.JobID, job.Produces); err != nil {
			slog.Warn("failed to pre-register --produces in artifact manifest",
				"component", "runner", "job_id", cfg.JobID, "error", err)
			WriteManifestErrorFile(paths, "pre-register: "+err.Error())
		}
	}

	// Cache probe (pre-job) should use the same HF env the job will run with.
	probeEnv := mergeEnvVars(os.Environ(), envVars)
	diskProbePath := resolveDiskProbePath(probeEnv, envValue(probeEnv, "HOME"), workingDir)
	if !cfg.SkipProbes {
		cachePre := ProbeCacheSizesForEnvAtPath(probeEnv, diskProbePath)
		phases.CachePre = &cachePre
	}

	// Resolve GPU devices
	var gpuDevices []string
	if job.GPUClass != "" {
		memPerDevice := GetJobGPUMem(job, DefaultGPUMemGB)
		// For single-job mode, use empty state — no other jobs running
		emptyState := NewState()
		device, ok := gpuInv.PickBestGPUForClass(emptyState, job.GPUClass, memPerDevice)
		if ok {
			gpuDevices = []string{device}
		}
	} else {
		gpuDevices = GetJobGPUDevices(job)
	}
	// Auto-assign GPU on GPU hosts when no explicit constraint was given.
	if len(gpuDevices) == 0 && len(gpuInv.Devices) > 0 {
		gpuInv.RefreshDeviceMemSnapshot()
		emptyState := NewState()
		if device := gpuInv.PickLeastLoadedGPU(emptyState); device != "" {
			gpuDevices = []string{device}
		}
	}
	if len(gpuDevices) > 0 {
		cudaEnv := FormatGPUDeviceEnv(gpuDevices)
		envVars = append(envVars, cudaEnv)
	}

	// Write gpu_devices to meta file
	if len(gpuDevices) > 0 {
		if f, err := os.OpenFile(paths.Meta, os.O_APPEND|os.O_WRONLY, 0644); err == nil {
			fmt.Fprintf(f, "gpu_devices=%s\n", strings.Join(gpuDevices, ","))
			f.Close()
		}
	}

	// Run setup command as a separate phase
	if setupCmd != "" {
		if cfg.SetupPrewarmed {
			slog.Info("skipping setup command: setup already prewarmed",
				"component", "runner", "job_id", cfg.JobID, "cmd", setupCmd)
		} else {
			ei, setupErr := RunSetupCommand(setupCmd, cfg.JobID, workingDir, envVars, paths, cfg.SetupTimeout)
			if setupErr != nil {
				now := time.Now().Unix()
				phases.SetupEnd = now

				// Early return skips the normal completion writes below;
				// without these the reconciler has no timestamps or logs.
				failureReason := DetectFailureReasonFromExitInfoAndLog(ei, paths.Log)
				WriteFailureReasonFile(paths, failureReason)
				WriteCompletionRecord(paths, ei, RunningJobState{}, "", failureReason, phases.SetupStart, now, nil)
				WritePhasesFile(paths, phases)

				return ei, setupErr
			}
			if setupCmd == "uv sync" {
				collectAndWriteUVManifest(cfg.JobID, expandedDir, cfg.LogDir)
			}
		}
	}

	phases.SetupEnd = time.Now().Unix()
	setupSecs := phases.SetupEnd - phases.SetupStart
	phases.SetupSeconds = &setupSecs

	// Start the process
	if cfg.OnPhase != nil {
		cfg.OnPhase("running")
	}
	phases.RunStart = time.Now().Unix()

	runCommand, prepRunErr := prepareUVRunCommand(expandedDir, setupCmd, command)
	if prepRunErr != nil {
		slog.Warn("uv run preflight failed; continuing with original command",
			"component", "runner", "job_id", cfg.JobID, "error", prepRunErr)
		appendSetupLog(paths.Log, []byte("weft: uv run preflight failed; continuing with original command: "+prepRunErr.Error()+"\n"))
	}
	if runCommand != command {
		appendSetupLog(paths.Log, []byte("weft: runtime command: "+runCommand+"\n"))
	}

	slog.Debug("launching process", "component", "runner", "job_id", cfg.JobID)
	proc, err := StartProcess(runCommand, workingDir, envVars, paths.Log)
	if err != nil {
		slog.Warn("job start failed", "component", "runner", "job_id", cfg.JobID, "error", err)
		ei := ExitInfo{ExitCode: 1}
		now := time.Now().Unix()
		phases.RunEnd = now
		os.WriteFile(paths.Status, []byte("1\n"), 0644)
		failureReason := fmt.Sprintf("start_process: %v", err)
		WriteFailureReasonFile(paths, failureReason)
		WriteCompletionRecord(paths, ei, RunningJobState{}, "", failureReason, phases.RunStart, now, nil)
		WritePhasesFile(paths, phases)
		return ei, fmt.Errorf("start process: %w", err)
	}

	proc.WritePIDFiles(paths)

	// Time budget enforcement: kill process group when deadline reached
	var timedOut atomic.Bool
	if cfg.MaxTime > 0 {
		timer := time.AfterFunc(cfg.MaxTime, func() {
			timedOut.Store(true)
			slog.Warn("max-time reached, sending SIGTERM", "component", "runner", "job_id", cfg.JobID, "max_time", cfg.MaxTime, "pgid", proc.PGID)
			KillProcessGroupWithGrace(proc.PGID, 10*time.Second, paths, "")
		})
		defer timer.Stop()
	}

	// Closed after Wait() so background goroutines can exit promptly.
	processDone := make(chan struct{})

	// Sampling loop in background
	samplingDone := make(chan struct{})
	var rs RunningJobState
	rs.StartedAt = phases.RunStart
	rs.GPUDevices = gpuDevices
	rs.GPUMemGB = GetJobGPUMem(job, DefaultGPUMemGB)
	rs.DiskPath = diskProbePath
	rs.TelemetryIntervalSeconds = int64(cfg.SampleInterval / time.Second)
	rs.TelemetryAdvancedGPU = telemetryPolicy.CollectAdvancedGPU

	takeSample := func() bool {
		if !CheckPIDAlive(proc.PID) {
			return false
		}
		SampleJob(proc.PID, proc.PGID, cpuCount, paths, &rs, "single")
		return true
	}

	go func() {
		defer close(samplingDone)

		// Immediate first sample so short jobs get at least one data point
		if !takeSample() {
			return
		}

		ticker := time.NewTicker(cfg.SampleInterval)
		defer ticker.Stop()

		for range ticker.C {
			if !takeSample() {
				return
			}
		}
	}()

	// Fatal log scanner: periodically check log tail for unrecoverable errors
	// (e.g., CUDA device lost) and kill the job early to avoid wasting rental time.
	var fatalError atomic.Bool
	fatalScanDone := make(chan struct{})
	if job.GPUClass != "" || len(gpuDevices) > 0 {
		go func() {
			defer close(fatalScanDone)
			scanTicker := time.NewTicker(30 * time.Second)
			defer scanTicker.Stop()

			var lastSize int64
			for {
				select {
				case <-processDone:
					return
				case <-scanTicker.C:
				}
				if !CheckPIDAlive(proc.PID) {
					return
				}
				info, err := os.Stat(paths.Log)
				if err != nil || info.Size() == lastSize {
					continue
				}
				lastSize = info.Size()
				tail := readLogTail(paths.Log, 16384)
				if tail == "" {
					continue
				}
				if d := remediation.CheckFatalAtRuntime(tail); d != nil {
					fatalError.Store(true)
					slog.Warn("fatal error detected in job logs, sending SIGTERM",
						"component", "runner", "job_id", cfg.JobID,
						"pattern", d.Pattern, "message", d.Message)
					KillProcessGroupWithGrace(proc.PGID, 10*time.Second, paths, d.Pattern)
					return
				}
			}
		}()
	} else {
		close(fatalScanDone)
	}

	// Hang-detection watchdogs. See specs/job-lifecycle.allium rules
	// GPUIdleKillsJob and StdoutSilenceKillsJob.
	grace := cfg.WatchdogInitialGrace
	if grace <= 0 {
		grace = 5 * time.Minute
	}
	runStartedAt := time.Now()

	startWatchdog := func(reason string, timeout time.Duration, hasActivity func() bool) (chan struct{}, *atomic.Bool) {
		done := make(chan struct{})
		fired := &atomic.Bool{}
		if timeout <= 0 {
			close(done)
			return done, fired
		}
		go func() {
			defer close(done)
			ticker := time.NewTicker(watchdogTickPeriod(timeout))
			defer ticker.Stop()
			var armed bool
			lastActivity := time.Now()
			for {
				select {
				case <-processDone:
					return
				case <-ticker.C:
				}
				if hasActivity() {
					lastActivity = time.Now()
					armed = true
				}
				if !armed && time.Since(runStartedAt) >= grace {
					armed = true
					lastActivity = time.Now()
				}
				if armed && time.Since(lastActivity) >= timeout {
					fired.Store(true)
					slog.Warn("watchdog firing, sending SIGTERM",
						"component", "runner", "job_id", cfg.JobID,
						"reason", reason, "pgid", proc.PGID)
					KillProcessGroupWithGrace(proc.PGID, 10*time.Second, paths, reason)
					return
				}
			}
		}()
		return done, fired
	}

	// Seed with header bytes from WriteLogHeader so the watchdog arms on
	// actual process output, not the pre-run header.
	silenceLastSize := int64(0)
	if info, err := os.Stat(paths.Log); err == nil {
		silenceLastSize = info.Size()
	}
	// Output-dir mtime updates (e.g. periodic checkpoint writes) count as a
	// sign of life alongside log growth. Seed at run start so files staged
	// before the run don't falsely arm the watchdog.
	outputDirs := job.OutputDirs
	if len(outputDirs) == 0 {
		outputDirs = config.DefaultOutputDirs
	}
	silenceOutputThreshold := time.Now()
	silenceDone, silenceKill := startWatchdog(KillReasonStdoutSilence, cfg.StdoutSilenceTimeout, func() bool {
		active := false
		if info, err := os.Stat(paths.Log); err == nil && info.Size() > silenceLastSize {
			silenceLastSize = info.Size()
			active = true
		}
		if anyOutputMtimeAfter(workingDir, outputDirs, silenceOutputThreshold) {
			silenceOutputThreshold = time.Now()
			active = true
		}
		return active
	})

	gpuIdleTimeout := cfg.GPUIdleTimeout
	if !JobHasExplicitGPUIntent(job) {
		gpuIdleTimeout = 0
	}
	probe := cfg.GPUActiveProbe
	if probe == nil {
		probe = func() bool { return HostGPUMetrics().UtilPct > 0 }
	}
	gpuIdleDone, gpuIdleKill := startWatchdog(KillReasonGPUIdle, gpuIdleTimeout, probe)

	// Wait for process
	waitErr := proc.Cmd.Wait()
	close(processDone)
	ei := ExtractExitInfo(waitErr)

	// Override exit info if we timed out (like GNU timeout exit code 124)
	if timedOut.Load() {
		ei.ExitCode = 124
		ei.Signaled = true
		ei.Signal = syscall.SIGTERM
		slog.Warn("job timed out", "component", "runner", "job_id", cfg.JobID, "max_time", cfg.MaxTime)
	}

	// Override exit info if killed due to fatal log error
	if fatalError.Load() {
		if ei.ExitCode == 0 {
			ei.ExitCode = 1
		}
		ei.Signaled = true
		ei.Signal = syscall.SIGTERM
	}

	if gpuIdleKill.Load() {
		ei.ExitCode = ExitCodeGPUIdleKill
		ei.Signaled = true
		ei.Signal = syscall.SIGTERM
	}
	if silenceKill.Load() {
		ei.ExitCode = ExitCodeStdoutSilenceKill
		ei.Signaled = true
		ei.Signal = syscall.SIGTERM
	}

	phases.RunEnd = time.Now().Unix()
	endTime := time.Now().Unix()

	// Stop sampling, fatal scanner, and hang watchdogs
	<-samplingDone
	<-fatalScanDone
	<-silenceDone
	<-gpuIdleDone

	// Write status and log footer
	WriteStatusFile(paths, ei)
	WriteLogFooter(paths, ei)

	// Failure detection
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

	// Discover outputs
	var outputFiles []OutputFile
	if ei.ExitCode == 0 {
		if err := RecordProducedArtifacts(cfg.JobID, job.Produces); err != nil {
			slog.Warn("failed to write artifact manifest from --produces",
				"component", "runner", "job_id", cfg.JobID, "error", err)
			WriteManifestErrorFile(paths, "post-exit: "+err.Error())
		}
		dirs := job.OutputDirs
		if len(dirs) == 0 {
			dirs = config.DefaultOutputDirs
		}
		if discovered, discErr := DiscoverOutputs(workingDir, dirs); discErr == nil && len(discovered) > 0 {
			outputFiles = discovered
			slog.Debug("discovered output files", "component", "runner", "job_id", cfg.JobID, "count", len(discovered))
		}
	}

	// Write rusage and completion record
	killReason := ReadKillReasonFile(paths.KillReason)
	WriteRusageFile(paths, rs)
	WriteCompletionRecord(paths, ei, rs, killReason, failureReason, phases.RunStart, endTime, outputFiles)

	// Cache probe (post-job) and write phases
	if !cfg.SkipProbes {
		cachePost := ProbeCacheSizesForEnvAtPath(probeEnv, diskProbePath)
		phases.CachePost = &cachePost
	}
	WritePhasesFile(paths, phases)

	CleanupPIDFiles(paths)

	slog.Info("job completed", "component", "runner", "job_id", cfg.JobID, "exit_code", ei.ExitCode)
	return ei, nil
}

func appendPrewarmLog(jobLog, prewarmLog string) {
	if prewarmLog == "" {
		return
	}
	data, err := os.ReadFile(prewarmLog)
	if err != nil || len(data) == 0 {
		return
	}
	f, err := os.OpenFile(jobLog, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n=== prewarmed setup ===\n")
	_, _ = f.Write(data)
	if data[len(data)-1] != '\n' {
		_, _ = f.WriteString("\n")
	}
	_, _ = f.WriteString("=== prewarmed setup complete ===\n")
}

// readLogTail reads the last maxBytes of a log file. Returns empty string on error.
func readLogTail(path string, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return ""
	}

	size := info.Size()
	if size == 0 {
		return ""
	}

	readSize := maxBytes
	offset := size - maxBytes
	if offset < 0 {
		offset = 0
		readSize = size
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}

	data := make([]byte, readSize)
	n, err := f.Read(data)
	if n == 0 || err != nil {
		return ""
	}
	return string(data[:n])
}
