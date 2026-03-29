package runner

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/opsqueue"
)

// SingleJobConfig configures a single-shot job execution.
type SingleJobConfig struct {
	JobID          int64
	Job            opsqueue.CommandJob
	LogDir         string
	WorkingDir     string             // Override job.Dir if non-empty
	SampleInterval time.Duration      // Default 1s
	MaxTime        time.Duration      // If >0, kill the job after this duration
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
	WriteMetaFile(paths, cfg.JobID, workingDir, command, job.Desc, "single", phases.WrapperStart)
	WriteLogHeader(paths, cfg.JobID, workingDir, command)

	// Build environment
	var envVars []string
	if expandedDir != "" {
		dotenvVars, _ := LoadDotenvFiles(expandedDir)
		envVars = append(envVars, dotenvVars...)
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
	if len(gpuDevices) > 0 && job.GPUClass != "" {
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
	if setupCmd := DetectSetupCommand(expandedDir); setupCmd != "" {
		ei, setupErr := RunSetupCommand(setupCmd, cfg.JobID, workingDir, envVars, paths)
		if setupErr != nil {
			phases.SetupEnd = time.Now().Unix()
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

	phases.RunEnd = time.Now().Unix()
	endTime := time.Now().Unix()

	// Stop sampling
	<-samplingDone

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
	WriteRusageFile(paths, rs)
	WriteCompletionRecord(paths, ei, rs, "", failureReason, phases.RunStart, endTime, outputFiles)

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
