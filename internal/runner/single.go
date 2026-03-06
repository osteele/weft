package runner

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/ops"
)

// SingleJobConfig configures a single-shot job execution.
type SingleJobConfig struct {
	JobID          int64
	Job            ops.CommandJob
	LogDir         string
	WorkingDir     string        // Override job.Dir if non-empty
	SampleInterval time.Duration // Default 15s
	SkipProbes     bool          // Skip cache size probes (useful in tests)
}

// RunSingleJob executes a single job synchronously with full telemetry.
// It handles GPU discovery, process management, sampling, failure detection,
// completion records, and output discovery — the same as the queue runner
// but without queue/scheduling logic.
func RunSingleJob(cfg SingleJobConfig) (ExitInfo, error) {
	if cfg.SampleInterval == 0 {
		cfg.SampleInterval = 15 * time.Second
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

	// Cache probe (pre-job)
	if !cfg.SkipProbes {
		cachePre := ProbeCacheSizes()
		phases.CachePre = &cachePre
	}

	// Discover hardware
	gpuInv := DiscoverGPUs()
	cpuCount := DetectCPUCount()

	// Setup phase
	phases.SetupStart = time.Now().Unix()

	os.MkdirAll(cfg.LogDir, 0755)

	paths := NewJobPaths(cfg.LogDir, cfg.JobID)

	// Archive existing files
	ArchiveExistingFiles(cfg.LogDir, cfg.JobID)

	// Expand ~ in working directory
	expandedDir := expandTilde(workingDir)

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

	// Detect and prepend setup command
	setupStart := time.Now()
	if setupCmd := DetectSetupCommand(expandedDir); setupCmd != "" {
		command = setupCmd + " && " + command
	}

	phases.SetupEnd = time.Now().Unix()
	setupSecs := int64(time.Since(setupStart).Seconds())
	phases.SetupSeconds = &setupSecs

	// Start the process
	phases.RunStart = time.Now().Unix()

	log.Printf("Job %d: launching process", cfg.JobID)
	proc, err := StartProcess(command, workingDir, envVars, paths.Log)
	if err != nil {
		log.Printf("Job %d: start failed: %v", cfg.JobID, err)
		os.WriteFile(paths.Status, []byte("1\n"), 0644)
		return ExitInfo{ExitCode: 1}, fmt.Errorf("start process: %w", err)
	}

	proc.WritePIDFiles(paths)

	// Sampling loop in background
	samplingDone := make(chan struct{})
	var rs RunningJobState
	rs.StartedAt = phases.RunStart
	rs.GPUDevices = gpuDevices
	rs.GPUMemGB = GetJobGPUMem(job, DefaultGPUMemGB)

	go func() {
		defer close(samplingDone)
		ticker := time.NewTicker(cfg.SampleInterval)
		defer ticker.Stop()

		for range ticker.C {
			now := time.Now()
			pid := proc.PID
			if !CheckPIDAlive(pid) {
				return
			}

			hostPct := ProcCPUHostPct(pid, cpuCount)
			WriteSample(paths, now.Unix(), hostPct, nil)

			// Resource usage from /proc
			rusagePID := pid
			if proc.PGID > 0 {
				rusagePID = proc.PGID
			}
			if _, err := os.Stat("/proc"); err == nil {
				userTicks, sysTicks, peakRSS := ProcResourceUsage(rusagePID)
				rs.RusageUserCPU = TicksToSeconds(userTicks)
				rs.RusageSysCPU = TicksToSeconds(sysTicks)
				if peakRSS > rs.RusagePeakRSS {
					rs.RusagePeakRSS = peakRSS
				}
			}

			// GPU memory sampling
			gpuMem := ProcGPUMemMiB(pid)
			if gpuMem > rs.RusageMaxGPU {
				rs.RusageMaxGPU = gpuMem
			}

			// Timeseries sample
			currentRSS := ProcCurrentRSSKB(rusagePID)
			hostTotal, hostUsed := HostMemoryKB()
			gpuUtil, gpuMemUsed, gpuMemTotal := HostGPUUtilization()
			pressure := MemoryPressureFromUsage(hostTotal, hostUsed)

			sample := TimeseriesSample{
				Ts:           now.Unix(),
				CPUPct:       hostPct,
				RSSKB:        currentRSS,
				GPUMiB:       gpuMem,
				HostRSSKB:    hostUsed,
				HostMemTotal: hostTotal,
				GPUUtilPct:   gpuUtil,
				GPUMemUsed:   gpuMemUsed,
				GPUMemTotal:  gpuMemTotal,
				MemPressure:  string(pressure),
				Tenant:       "single",
			}
			WriteTimeseriesSample(paths, sample)

			if MemPressureSeverity(pressure) > MemPressureSeverity(rs.PeakMemPressure) {
				rs.PeakMemPressure = pressure
			}
			if hostTotal > 0 {
				ratio := float64(hostUsed) / float64(hostTotal)
				if ratio > rs.PeakHostMemRatio {
					rs.PeakHostMemRatio = ratio
				}
			}
			if currentRSS > rs.PeakRSSFromTS {
				rs.PeakRSSFromTS = currentRSS
			}

			rs.LastHeartbeat = now.Unix()
			rs.LastSample = now.Unix()
			WriteHeartbeat(paths, now.Unix())
		}
	}()

	// Wait for process
	waitErr := proc.Cmd.Wait()
	ei := ExtractExitInfo(waitErr)

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
			log.Printf("Job %d: discovered %d output files", cfg.JobID, len(discovered))
		}
	}

	// Write rusage and completion record
	WriteRusageFile(paths, rs)
	WriteCompletionRecord(paths, ei, rs, "", failureReason, phases.RunStart, endTime, outputFiles)

	// Cache probe (post-job) and write phases
	if !cfg.SkipProbes {
		cachePost := ProbeCacheSizes()
		phases.CachePost = &cachePost
	}
	WritePhasesFile(paths, phases)

	CleanupPIDFiles(paths)

	log.Printf("Job %d: completed (exit=%d)", cfg.JobID, ei.ExitCode)
	return ei, nil
}
