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
	JobID          int64
	Job            opsqueue.CommandJob
	LogDir         string
	WorkingDir     string             // Override job.Dir if non-empty
	SampleInterval time.Duration      // Default 1s
	MaxTime        time.Duration      // If >0, kill the job after this duration
	SetupTimeout   time.Duration      // If >0, kill the setup command after this duration (default 20m)
	SkipProbes     bool               // Skip cache size probes (useful in tests)
	OnPhase        func(phase string) // Called at phase transitions: "setup", "running"
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
	WriteMetaFile(paths, cfg.JobID, workingDir, command, job.Desc, "single", phases.WrapperStart, "")
	WriteLogHeader(paths, cfg.JobID, workingDir, command, "")

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

			failureReason := DetectFailureReasonFromExitInfo(ei)
			WriteFailureReasonFile(paths, failureReason)
			WriteCompletionRecord(paths, ei, RunningJobState{}, "", failureReason, phases.SetupStart, now, nil)
			WritePhasesFile(paths, phases)

			return ei, resolveErr
		}
		envVars = resolvedEnv
		setupCmd = ""
	}
	envVars = append(envVars, job.Env...)
	envVars = artifacts.MergeEnvVars(envVars, cfg.JobID)

	// Cache probe (pre-job) should use the same HF env the job will run with.
	if !cfg.SkipProbes {
		cachePre := ProbeCacheSizesForEnv(mergeEnvVars(os.Environ(), envVars))
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
		ei, setupErr := RunSetupCommand(setupCmd, cfg.JobID, workingDir, envVars, paths, cfg.SetupTimeout)
		if setupErr != nil {
			now := time.Now().Unix()
			phases.SetupEnd = now

			// Early return skips the normal completion writes below;
			// without these the reconciler has no timestamps or logs.
			failureReason := DetectFailureReasonFromExitInfo(ei)
			WriteFailureReasonFile(paths, failureReason)
			WriteCompletionRecord(paths, ei, RunningJobState{}, "", failureReason, phases.SetupStart, now, nil)
			WritePhasesFile(paths, phases)

			return ei, setupErr
		}
		if setupCmd == "uv sync" {
			collectAndWriteUVManifest(cfg.JobID, expandedDir, cfg.LogDir)
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

	slog.Debug("launching process", "component", "runner", "job_id", cfg.JobID)
	proc, err := StartProcess(command, workingDir, envVars, paths.Log)
	if err != nil {
		slog.Warn("job start failed", "component", "runner", "job_id", cfg.JobID, "error", err)
		os.WriteFile(paths.Status, []byte("1\n"), 0644)
		return ExitInfo{ExitCode: 1}, fmt.Errorf("start process: %w", err)
	}

	proc.WritePIDFiles(paths)

	// Time budget enforcement: kill process group when deadline reached
	var timedOut atomic.Bool
	if cfg.MaxTime > 0 {
		timer := time.AfterFunc(cfg.MaxTime, func() {
			timedOut.Store(true)
			slog.Warn("max-time reached, sending SIGTERM", "component", "runner", "job_id", cfg.JobID, "max_time", cfg.MaxTime, "pgid", proc.PGID)
			syscall.Kill(-proc.PGID, syscall.SIGTERM)
			// Give the process a grace period to clean up, then SIGKILL
			time.AfterFunc(10*time.Second, func() {
				syscall.Kill(-proc.PGID, syscall.SIGKILL)
			})
		})
		defer timer.Stop()
	}

	// Sampling loop in background
	samplingDone := make(chan struct{})
	var rs RunningJobState
	rs.StartedAt = phases.RunStart
	rs.GPUDevices = gpuDevices
	rs.GPUMemGB = GetJobGPUMem(job, DefaultGPUMemGB)
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
			for range scanTicker.C {
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
					WriteKillReasonFile(paths, d.Pattern)
					syscall.Kill(-proc.PGID, syscall.SIGTERM)
					time.AfterFunc(10*time.Second, func() {
						syscall.Kill(-proc.PGID, syscall.SIGKILL)
					})
					return
				}
			}
		}()
	} else {
		close(fatalScanDone)
	}

	// Wait for process
	waitErr := proc.Cmd.Wait()
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

	phases.RunEnd = time.Now().Unix()
	endTime := time.Now().Unix()

	// Stop sampling and fatal scanner
	<-samplingDone
	<-fatalScanDone

	// Write status and log footer
	WriteStatusFile(paths, ei)
	WriteLogFooter(paths, ei)

	// Failure detection
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

	// Discover outputs
	var outputFiles []OutputFile
	if ei.ExitCode == 0 {
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
		cachePost := ProbeCacheSizesForEnv(mergeEnvVars(os.Environ(), envVars))
		phases.CachePost = &cachePost
	}
	WritePhasesFile(paths, phases)

	CleanupPIDFiles(paths)

	slog.Info("job completed", "component", "runner", "job_id", cfg.JobID, "exit_code", ei.ExitCode)
	return ei, nil
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
