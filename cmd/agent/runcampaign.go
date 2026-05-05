package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/progress"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/retry"
	"github.com/osteele/weft/internal/runner"
)

// runCampaign implements the "run-campaign" subcommand.
// Usage: weft-agent run-campaign --r2-bucket=BUCKET --instance-id=ID [--log-dir=/tmp/weft-logs] [--max-time=2h] [--grace-period=15m]
func runCampaign(args []string) {
	var r2Bucket string
	var instanceID string
	var logDir string
	var maxTime time.Duration
	var gracePeriod time.Duration
	var skipWorkdirDeletion bool

	for _, arg := range args {
		switch {
		case hasPrefix(arg, "--r2-bucket="):
			r2Bucket = arg[len("--r2-bucket="):]
		case hasPrefix(arg, "--instance-id="):
			instanceID = arg[len("--instance-id="):]
		case hasPrefix(arg, "--log-dir="):
			logDir = arg[len("--log-dir="):]
		case hasPrefix(arg, "--max-time="):
			val := arg[len("--max-time="):]
			var err error
			maxTime, err = time.ParseDuration(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid --max-time: %s\n", val)
				os.Exit(1)
			}
		case hasPrefix(arg, "--grace-period="):
			val := arg[len("--grace-period="):]
			var err error
			gracePeriod, err = time.ParseDuration(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid --grace-period: %s\n", val)
				os.Exit(1)
			}
		case arg == "--skip-workdir-deletion":
			skipWorkdirDeletion = true
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", arg)
			os.Exit(1)
		}
	}

	if r2Bucket == "" || instanceID == "" {
		fmt.Fprintln(os.Stderr, "required: --r2-bucket, --instance-id")
		os.Exit(1)
	}
	if logDir == "" {
		logDir = "/tmp/weft-logs"
	}
	_ = os.MkdirAll(logDir, 0o755)

	oplogPath := filepath.Join(logDir, agentOpslogFile)
	if err := oplog.Init(oplogPath, 0); err != nil {
		fmt.Fprintf(os.Stderr, "warning: init opslog: %v\n", err)
	}
	defer oplog.Close()

	instanceIDInt, err := strconv.ParseInt(instanceID, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid instance ID %q: %v\n", instanceID, err)
		os.Exit(1)
	}
	oplog.Log(oplog.OpAgentStart, oplog.WithDetailf("run-campaign instance=%s", instanceID))

	// Fetch manifest from R2
	manifestKey := r2keys.CampaignManifest(instanceIDInt)
	manifestJSON, err := r2Get(r2Bucket, manifestKey)
	if err != nil || manifestJSON == "" {
		fmt.Fprintf(os.Stderr, "failed to fetch manifest from R2 key %s: %v\n", manifestKey, err)
		os.Exit(1)
	}

	var manifest cloud.CampaignManifest
	if err := json.Unmarshal([]byte(manifestJSON), &manifest); err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse manifest: %v\n", err)
		os.Exit(1)
	}

	// Export env vars from manifest
	for k, v := range manifest.Env {
		os.Setenv(k, v)
	}

	// R2 key for instance phase tracking
	phaseKey := r2keys.InstancePhase(instanceIDInt)

	// Track current phase for heartbeat reporting
	var currentPhase syncString
	currentPhase.Set("starting")
	diskPath := campaignDiskPath(manifest.Jobs)
	phaseFile := filepath.Join(logDir, "instance-phase.txt")
	fatalFile := filepath.Join(logDir, "agent-fatal.txt")
	setAgentFatalFile(fatalFile)
	writeLocalPhaseFile(phaseFile, currentPhase.Get())
	// Capture panics in the main goroutine: silent agent death produces no
	// signal beyond a heartbeat gap, leaving the reconciler unable to
	// distinguish "agent crashed" from "container/network blackholed". The
	// fatal file is read by the sidecar on its next tick and propagates as
	// agent_fatal in the heartbeat sample.
	defer func() {
		if r := recover(); r != nil {
			detail := fmt.Sprintf("runCampaign main panic: %v", r)
			oplog.Log(oplog.OpAgentPanic, oplog.WithDetail(detail))
			fmt.Fprintln(os.Stderr, detail)
			writeAgentFatal(detail)
			panic(r)
		}
	}()

	startTime := time.Now()
	anyFailed := false

	// Detect docker-restart-on-same-disk via a sentinel file. The sentinel
	// is written below this block on every agent start; if it already exists
	// at boot, the container has restarted (typical of a Vast.ai
	// pause/resume on an interruptible instance). The sentinel survives
	// docker stop because it lives on the instance disk.
	resumed := false
	const resumeSentinel = "/var/run/weft-agent-started"
	if _, err := os.Stat(resumeSentinel); err == nil {
		resumed = true
	} else {
		_ = os.MkdirAll(filepath.Dir(resumeSentinel), 0o755)
		_ = os.WriteFile(resumeSentinel, []byte(startTime.UTC().Format(time.RFC3339)), 0o644)
	}

	// Upload agent startup timestamp to R2 for boot timing analysis
	fatalAgentGo("agent-startup-upload", func() {
		payload, _ := json.Marshal(map[string]int64{"agent_start_unix": startTime.Unix()})
		_ = r2Put(r2Bucket, r2keys.InstanceAgentStartup(instanceIDInt), string(payload))
	})

	// Start heartbeat reporter (writes host metrics to R2 every 30s, plus
	// an immediate first heartbeat and one on every phase transition).
	stopHeartbeatSidecar := startHeartbeatSidecarProcess(r2Bucket, instanceIDInt, diskPath, phaseFile, fatalFile)
	defer stopHeartbeatSidecar()
	forceHeartbeat, stopHeartbeat := startHeartbeatReporter(r2Bucket, instanceIDInt, diskPath, currentPhase.Get)
	defer stopHeartbeat()
	// onPhase wraps currentPhase.Set so each transition also forces an
	// out-of-band heartbeat upload — a phase change is exactly when we
	// most want fresh disk and metric state on R2.
	onPhase := func(phase string) {
		currentPhase.Set(phase)
		writeLocalPhaseFile(phaseFile, phase)
		forceHeartbeat()
	}
	stopOpslogReporter := startOpslogReporter(r2Bucket, instanceIDInt, logDir)
	defer stopOpslogReporter()
	stopDiskMonitor := startDiskMonitor(r2Bucket, instanceIDInt, diskPath, logDir, phaseKey, manifest.SelfDestructCmd, currentPhase.Get, onPhase)
	defer stopDiskMonitor()

	// Reclaim disk from prior tenants on instance reuse: drop any HF
	// cache entries that aren't declared by the current manifest. Done
	// before the disk-cap probe so the probe sees post-purge totals.
	declaredInputs := collectDeclaredInputs(manifest.Jobs)
	if len(declaredInputs) > 0 {
		if purged, freed := purgeUndeclaredHFAssets(declaredInputs); purged > 0 {
			fmt.Printf("hf cache hygiene: purged %d undeclared assets, freed %s\n",
				purged, formatBytes(freed))
		}
	}

	// Verify the provider actually allocated the container disk we asked
	// for. Vast.ai (and others) can silently cap the request on a
	// multi-tenant host. Failing fast here avoids running for hours and
	// hitting ENOSPC mid-job.
	if checkDiskCap(r2Bucket, instanceIDInt, manifest.RequestedDiskGB, diskPath, manifest.SelfDestructCmd) {
		return
	}

	seqResult := runJobSequence(manifest.Jobs, jobSequenceConfig{
		R2Bucket:            r2Bucket,
		InstanceID:          instanceIDInt,
		PhaseKey:            phaseKey,
		LogDir:              logDir,
		DiskPath:            diskPath,
		MaxTime:             maxTime,
		StartTime:           startTime,
		OnPhase:             onPhase,
		SkipWorkdirDeletion: manifest.SkipWorkdirDeletion || skipWorkdirDeletion,
		GPUWarmup:           manifest.GPUWarmup,
		CostPerHourCents:    manifest.CostPerHourCents,
		Provider:            manifest.Provider,
		InstanceType:        manifest.InstanceType,
		Resumed:             resumed,
	})
	anyFailed = seqResult.AnyFailed

	// Grace period or self-destruct
	if anyFailed && gracePeriod > 0 {
		fmt.Printf("Jobs failed. Entering grace period (%s).\n", gracePeriod)
		graceWaitLoop(graceWaitConfig{
			InstanceID:          instanceID,
			R2Bucket:            r2Bucket,
			Timeout:             gracePeriod,
			SelfDestructCmd:     manifest.SelfDestructCmd,
			LogDir:              logDir,
			SkipWorkdirDeletion: manifest.SkipWorkdirDeletion || skipWorkdirDeletion,
		})
	} else {
		terminalStatus := db.LaunchStatusCompleted
		terminationReason := db.TerminationReasonCompleted
		if anyFailed {
			terminalStatus = db.LaunchStatusFailed
			terminationReason = db.TerminationReasonJobFailure
		}
		cm := collectCompletionManifest(logDir, manifest.Jobs)
		selfDestruct(selfDestructOpts{
			Bucket: r2Bucket, InstanceID: instanceID, SelfDestructCmd: manifest.SelfDestructCmd,
			TerminalStatus: terminalStatus, TerminationReason: terminationReason,
			Phase: currentPhase.Get(), CompletionManifest: cm,
		})
	}
}

func campaignDiskPath(jobs []cloud.AgentJob) string {
	for _, job := range jobs {
		if job.Dir == "" {
			continue
		}
		parent := filepath.Dir(job.Dir)
		if parent != "" && parent != "." {
			return parent
		}
		return job.Dir
	}
	return "/"
}

// checkForNewJobs drains any queued grace-job requests and returns the jobs.
// This enables running instances to pick up jobs submitted via campaign reuse
// without losing requests that arrive before the next poll.
func checkForNewJobs(r2Bucket string, instanceID int64, onPhase func(string)) []cloud.AgentJob {
	jobs, err := drainGraceJobRequests(r2Bucket, instanceID, onPhase)
	if err != nil {
		fmt.Fprintf(os.Stderr, "check for new jobs: %v\n", err)
		return nil
	}
	return jobs
}

// writePhase writes a phase marker to R2 in a background goroutine.
// Best-effort is intentional: coordinator-side phase reconciliation is DB-driven
// for the running state, so dropped phase PUTs only affect transient sub-state display.
func writePhase(r2Bucket, phaseKey, phase string) {
	go r2Put(r2Bucket, phaseKey, phase)
}

// phaseCallback returns an OnPhase callback that writes phase markers to R2.
func phaseCallback(r2Bucket, phaseKey string, jobID int64, setPhase func(string)) func(string) {
	return func(phase string) {
		full := fmt.Sprintf("%s:%d", phase, jobID)
		if setPhase != nil {
			setPhase(full)
		}
		oplog.Log(oplog.OpPhaseTransition, oplog.WithJobID(jobID), oplog.WithDetail(full))
		writePhase(r2Bucket, phaseKey, full)
	}
}

// rcloneMinTimeout is the floor for any single rclone invocation. Even small
// uploads should tolerate brief network stalls.
const rcloneMinTimeout = 5 * time.Minute

// rcloneBytesPerSecondFloor is the assumed worst-case sustained throughput
// used to scale per-call rclone timeouts. ~1 MB/s is conservative for cloud
// rental networks where R2 uploads can be bursty.
const rcloneBytesPerSecondFloor = 1 << 20

// rcloneTimeoutForBytes returns a per-rclone-call deadline that scales with
// the payload size, so a 500 MB checkpoint is not killed by a fixed 5-minute
// ceiling on a slow link. Callers pass 0 if size is unknown to get the floor.
func rcloneTimeoutForBytes(bytes int64) time.Duration {
	if bytes <= 0 {
		return rcloneMinTimeout
	}
	scaled := time.Duration(bytes/rcloneBytesPerSecondFloor) * time.Second
	if scaled < rcloneMinTimeout {
		return rcloneMinTimeout
	}
	return scaled
}

// uploadOutputDirs uploads convention-based output directories to R2.
func uploadOutputDirs(bucket string, jobID, runID int64, workDir string) runner.OutputUploadResult {
	startedAt := time.Now()
	var result runner.OutputUploadResult
	var attempted int
	var failed int
	var totalDuration time.Duration
	workDir = runner.ExpandTilde(workDir)

	for _, dir := range config.DefaultOutputDirs {
		dir = strings.TrimRight(dir, "/")
		dirPath := filepath.Join(workDir, dir)
		info, err := os.Stat(dirPath)
		if err != nil || !info.IsDir() {
			continue
		}
		attempted++
		fileCount, bytes := measureUploadTree(dirPath)
		timeout := rcloneTimeoutForBytes(bytes)
		start := time.Now()
		retryCount := 0
		lastErr := retry.Do(context.Background(), retry.ExplicitDelays(5*time.Second, 10*time.Second), func() error {
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			cmd := exec.CommandContext(ctx, "rclone", "copy",
				"--update",
				dirPath+"/",
				"r2:"+bucket+"/"+r2keys.JobAttemptOutputDir(jobID, runID, dir),
			)
			cmd.Stderr = os.Stderr
			return cmd.Run()
		}, retry.WithOnRetry(func(attempt int, err error, delay time.Duration) {
			retryCount++
			fmt.Fprintf(os.Stderr, "upload outputs %s for job %d (attempt %d/3): %v; retrying in %s\n",
				dir, jobID, attempt, err, delay)
		}))
		if lastErr != nil {
			duration := time.Since(start)
			totalDuration += duration
			fmt.Fprintf(os.Stderr, "upload outputs %s for job %d failed: %v\n", dir, jobID, lastErr)
			oplog.Log(oplog.OpR2Copy, oplog.WithJobID(jobID),
				oplog.WithDetailf("output dir=%s", dir), oplog.WithError(lastErr),
				oplog.WithDuration(duration))
			result.Dirs = append(result.Dirs, runner.OutputDirUpload{
				Dir:        dir,
				Status:     runner.UploadStatusFailed,
				Error:      lastErr.Error(),
				FileCount:  fileCount,
				Bytes:      bytes,
				RetryCount: retryCount,
				DurationMS: duration.Milliseconds(),
			})
			result.FileCount += fileCount
			result.Bytes += bytes
			result.RetryCount += retryCount
			failed++
			continue
		}
		duration := time.Since(start)
		totalDuration += duration
		oplog.Log(oplog.OpR2Copy, oplog.WithJobID(jobID),
			oplog.WithDetailf("output dir=%s", dir),
			oplog.WithDuration(duration))
		result.FileCount += fileCount
		result.Bytes += bytes
		result.RetryCount += retryCount
		result.Dirs = append(result.Dirs, runner.OutputDirUpload{
			Dir:        dir,
			Status:     runner.UploadStatusOK,
			FileCount:  fileCount,
			Bytes:      bytes,
			RetryCount: retryCount,
			DurationMS: duration.Milliseconds(),
		})
	}

	finalizeUploadResult(&result, attempted, failed, startedAt, totalDuration)
	return result
}

// uploadArtifactManifestEntries reads the WEFT_ARTIFACT_MANIFEST written by the
// job script and uploads each declared artifact file (plus the manifest itself)
// to R2 under the artifacts/ prefix for this job attempt.
func uploadArtifactManifestEntries(bucket string, jobID, runID int64, workDir string) runner.OutputUploadResult {
	startedAt := time.Now()
	manifestPath := runner.ExpandTilde(artifacts.RemoteManifestPath(jobID))
	manifest, err := artifacts.ReadManifestFile(manifestPath, jobID)
	if err != nil || len(manifest.Artifacts) == 0 {
		return runner.OutputUploadResult{Status: runner.UploadStatusOK}
	}

	manifestData, err := json.Marshal(manifest)
	if err == nil {
		manifestKey := r2keys.JobAttemptArtifactManifest(jobID, runID)
		if putErr := r2Put(bucket, manifestKey, string(manifestData)); putErr != nil {
			fmt.Fprintf(os.Stderr, "upload artifact manifest for job %d: %v\n", jobID, putErr)
		}
	}

	root := artifacts.ResolveArtifactRoot(manifest, workDir)
	root = runner.ExpandTilde(root)
	filesPrefix := r2keys.JobAttemptArtifactFilesPrefix(jobID, runID)

	var result runner.OutputUploadResult
	var attempted, failed int
	var totalDuration time.Duration

	for _, spec := range manifest.Artifacts {
		if strings.TrimSpace(spec.Path) == "" {
			continue
		}
		remotePath := artifacts.ResolveRemotePath(root, spec.Path)
		remotePath = runner.ExpandTilde(remotePath)

		info, statErr := os.Stat(remotePath)
		if statErr != nil {
			fmt.Fprintf(os.Stderr, "artifact %q for job %d: %v\n", spec.Path, jobID, statErr)
			continue
		}
		attempted++
		var (
			src, dest, rcloneCmd string
			fileCount            int
			bytes                int64
		)
		localRel := artifacts.LocalRelativePath(spec.Path)
		if info.IsDir() {
			fileCount, bytes = measureUploadTree(remotePath)
			src, dest, rcloneCmd = remotePath+"/", filesPrefix+localRel+"/", "copy"
		} else {
			fileCount, bytes = 1, info.Size()
			src, dest, rcloneCmd = remotePath, filesPrefix+localRel, "copyto"
		}
		start := time.Now()
		uploadErr := rcloneUploadWithRetry(bucket, src, dest, rcloneCmd, jobID, spec.Path, bytes)
		duration := time.Since(start)
		totalDuration += duration
		status := runner.UploadStatusOK
		var errStr string
		if uploadErr != nil {
			failed++
			status = runner.UploadStatusFailed
			errStr = uploadErr.Error()
		}
		result.Dirs = append(result.Dirs, runner.OutputDirUpload{
			Dir: spec.Path, Status: status, Error: errStr,
			FileCount: fileCount, Bytes: bytes, DurationMS: duration.Milliseconds(),
		})
		result.FileCount += fileCount
		result.Bytes += bytes
	}

	finalizeUploadResult(&result, attempted, failed, startedAt, totalDuration)
	return result
}

// rcloneUploadWithRetry runs an rclone command with retries on transient failures.
// sizeBytes scales the per-call timeout; pass 0 if unknown.
func rcloneUploadWithRetry(bucket, src, r2Key, rcloneCmd string, jobID int64, label string, sizeBytes int64) error {
	timeout := rcloneTimeoutForBytes(sizeBytes)
	return retry.Do(context.Background(), retry.ExplicitDelays(5*time.Second, 10*time.Second), func() error {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		args := []string{rcloneCmd}
		if rcloneCmd == "copy" {
			args = append(args, "--update")
		}
		args = append(args, src, "r2:"+bucket+"/"+r2Key)
		cmd := exec.CommandContext(ctx, "rclone", args...)
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}, retry.WithOnRetry(func(attempt int, err error, delay time.Duration) {
		fmt.Fprintf(os.Stderr, "upload %s for job %d (attempt %d/3): %v; retrying in %s\n",
			label, jobID, attempt, err, delay)
	}))
}

// finalizeUploadResult sets status and timing fields on an OutputUploadResult.
func finalizeUploadResult(result *runner.OutputUploadResult, attempted, failed int, startedAt time.Time, totalDuration time.Duration) {
	switch {
	case attempted == 0 || failed == 0:
		result.Status = runner.UploadStatusOK
	case failed == attempted:
		result.Status = runner.UploadStatusFailed
	default:
		result.Status = runner.UploadStatusPartial
	}
	result.DurationMS = totalDuration.Milliseconds()
	if attempted > 0 {
		result.StartedAtUnix = startedAt.Unix()
		result.CompletedAtUnix = time.Now().Unix()
	}
}

func measureUploadTree(root string) (files int, bytes int64) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes
}

// promoteUVManifest reads uv-manifest.json from logDir and copies it to
// a content-addressable R2 key under uv-manifests/<hash>/<platform>.json.
func promoteUVManifest(bucket, logDir string) {
	manifestPath := filepath.Join(logDir, "uv-manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return // no manifest = nothing to do
	}
	var m runner.UVManifest
	if json.Unmarshal(data, &m) != nil || m.LockfileHash == "" || m.Platform == "" {
		return
	}
	if len(m.Packages) == 0 {
		// An empty package list (e.g. from a `uv run --script` invocation that
		// did not populate the project's wheel cache) would poison the
		// content-addressable disk estimator for this lockfile. Skip upload.
		slog.Warn("skipping upload of empty uv manifest",
			"component", "agent", "lockfile_hash", m.LockfileHash, "platform", m.Platform)
		return
	}
	key := r2keys.UVManifest(m.LockfileHash, m.Platform)
	r2Put(bucket, key, string(data))
}

// cleanLogDir removes all contents of the log directory to prepare for the next job.
func cleanLogDir(logDir string) {
	os.RemoveAll(logDir)
	os.MkdirAll(logDir, 0o755)
}

// runJobWithProgress runs a single job with progress reporting to R2.
// Starts a background goroutine that tails the log for progress lines,
// cleans up the R2 progress key when done.
func runJobWithProgress(r2Bucket string, jobID, runID, instanceID int64, logDir string, cfg runner.SingleJobConfig) (runner.ExitInfo, error) {
	logPath := filepath.Join(logDir, fmt.Sprintf("%d.log", jobID))
	stopProgress := startProgressReporter(r2Bucket, jobID, runID, logPath)
	stopLogs := startLogUploader(r2Bucket, jobID, runID, logPath)
	stopOutputs := startOutputUploader(r2Bucket, jobID, runID, cfg.WorkingDir)
	stopKillPoller := startKillPoller(r2Bucket, instanceID, jobID, logDir)
	defer func() {
		stopKillPoller()
		stopProgress()
		stopLogs()
		stopOutputs()
		r2Delete(r2Bucket, r2keys.JobAttemptProgress(jobID, runID))
	}()
	return runner.RunSingleJob(cfg)
}

// startKillPoller polls R2 for a kill signal targeting the current job.
// When the CLI writes the job ID to instance/<id>/kill-job, the poller
// reads the pgid file from logDir and sends SIGTERM/SIGKILL to the process group.
func startKillPoller(r2Bucket string, instanceID, jobID int64, logDir string) func() {
	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }

	fatalAgentGo("kill-poller", func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				val, err := r2Get(r2Bucket, r2keys.InstanceKillJob(instanceID))
				if err != nil {
					fmt.Fprintf(os.Stderr, "poll kill signal for job %d: %v\n", jobID, err)
					continue
				}
				if val == "" {
					continue
				}
				targetJobID, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
				if err != nil || targetJobID != jobID {
					continue
				}

				fmt.Printf("Kill signal received for job %d via R2\n", jobID)
				oplog.Log(oplog.OpJobKill, oplog.WithJobID(jobID), oplog.WithDetail("kill signal from R2"))

				pgidPath := filepath.Join(logDir, fmt.Sprintf("%d.pgid", jobID))
				if pgid, ok := runner.ReadPIDFile(pgidPath); ok {
					runner.WriteKillReasonFile(runner.NewJobPaths(logDir, jobID), runner.KillReasonUserKill)
					runner.KillProcessGroup(pgid)
				}

				r2Delete(r2Bucket, r2keys.InstanceKillJob(instanceID))
				return
			}
		}
	})

	return stop
}

// startProgressReporter starts a goroutine that periodically reads the job log,
// parses progress lines, and writes the percent to R2. Returns a stop function.
func startProgressReporter(r2Bucket string, jobID, runID int64, logPath string) func() {
	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }

	fatalAgentGo("progress-reporter", func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		tracker := progress.NewPhaseTracker()
		lastValue := ""

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				tail := readLogTail(logPath, 8192)
				if tail == "" {
					continue
				}
				prog := progress.FindLastProgressPreferExplicit(tail)
				if prog == nil {
					continue
				}
				phase, pct := tracker.Update(prog.DisplayPercent())
				if pct < 0 {
					continue
				}
				// Format: "pct" for phase 1, "phase:pct" for subsequent phases
				value := fmt.Sprintf("%d", pct)
				if phase > 1 {
					value = fmt.Sprintf("%d:%d", phase, pct)
				}
				if value != lastValue {
					lastValue = value
					r2Put(r2Bucket, r2keys.JobAttemptProgress(jobID, runID), value)
				}
			}
		}
	})

	return stop
}

// startHeartbeatReporter starts a goroutine that writes host-level metrics
// to R2 every 30 seconds. getPhase returns the current instance phase string.
//
// The first heartbeat is emitted synchronously before this function returns,
// so the post-mortem can distinguish "instance died before agent_ready" from
// "instance died during the first observable phase" — the 30-second tick
// interval would otherwise leave a window in which an instance that fails
// during early bootstrap (uv sync, model downloads, etc.) leaves no
// heartbeat at all on R2.
//
// Returns a tuple of (force, stop): `force` triggers an immediate
// out-of-band heartbeat (call this on phase transitions so phase changes
// land on R2 within network-latency rather than within the 30-second
// tick); `stop` halts the ticker.
func startHeartbeatReporter(r2Bucket string, instanceID int64, diskPath string, getPhase func() string) (force func(), stop func()) {
	heartbeatKey := r2keys.InstanceHeartbeat(instanceID)
	emit := func() {
		sample := collectHeartbeat(getPhase(), diskPath)
		data, err := json.Marshal(sample)
		if err != nil {
			return
		}
		_ = r2Put(r2Bucket, heartbeatKey, string(data))
	}
	return startHeartbeatReporterWithEmit(emit)
}

// startHeartbeatReporterWithEmit is the testable core of startHeartbeatReporter:
// it runs the same first-pulse-then-tick loop while letting the test inject a
// counter or buffer in place of the real R2 PUT.
func startHeartbeatReporterWithEmit(emit func()) (force func(), stop func()) {
	var once sync.Once
	done := make(chan struct{})
	stop = func() { once.Do(func() { close(done) }) }
	safeEmit := func() {
		defer func() {
			if r := recover(); r != nil {
				oplog.Log(oplog.OpAgentHeartbeat, oplog.WithDetailf("panic: %v", r))
			}
		}()
		emit()
	}

	// Synchronous first heartbeat: the cost (a single R2 PUT during
	// startup) is negligible compared to the diagnostic value when an
	// instance dies before the first ticker tick.
	safeEmit()

	pulse := make(chan struct{}, 1)
	force = func() {
		select {
		case pulse <- struct{}{}:
		default:
		}
	}

	fatalAgentGo("heartbeat-reporter", func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				safeEmit()
			case <-pulse:
				safeEmit()
			}
		}
	})

	return force, stop
}

// startOpslogReporter starts a goroutine that periodically uploads the agent
// opslog to R2 while jobs are still running.
func startOpslogReporter(bucket string, instanceID int64, logDir string) func() {
	var once sync.Once
	done := make(chan struct{})
	stopped := make(chan struct{})
	stop := func() {
		once.Do(func() { close(done) })
		<-stopped
	}

	fatalAgentGo("opslog-reporter", func() {
		defer close(stopped)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				uploadOpslog(bucket, instanceID, logDir)
				return
			case <-ticker.C:
				uploadOpslog(bucket, instanceID, logDir)
			}
		}
	})

	return stop
}

// startTimeseriesUploader starts a goroutine that periodically uploads the
// local timeseries JSONL file to a live R2 checkpoint key.
func startTimeseriesUploader(bucket string, jobID, runID int64, timeseriesPath string) func() {
	return startFileUploader(bucket, jobID, timeseriesPath, r2keys.JobAttemptLiveTimeseries(jobID, runID), "live timeseries")
}

// startTelemetryUploader starts a goroutine that periodically uploads the
// richer telemetry JSONL file to a live R2 checkpoint key.
func startTelemetryUploader(bucket string, jobID, runID int64, telemetryPath string) func() {
	return startFileUploader(bucket, jobID, telemetryPath, r2keys.JobAttemptLiveTelemetry(jobID, runID), "live telemetry")
}

// startOutputUploader periodically uploads changed output/checkpoint files for
// resilience on interruptible instances. A final sync runs when stopped.
func startOutputUploader(bucket string, jobID, runID int64, workDir string) func() {
	var once sync.Once
	done := make(chan struct{})
	stopped := make(chan struct{})
	stop := func() {
		once.Do(func() { close(done) })
		<-stopped
	}

	workDir = runner.ExpandTilde(workDir)
	upload := func() {
		_ = uploadOutputDirs(bucket, jobID, runID, workDir)
		_ = uploadArtifactManifestEntries(bucket, jobID, runID, workDir)
	}

	fatalAgentGo("output-uploader", func() {
		defer close(stopped)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				upload()
				return
			case <-ticker.C:
				upload()
			}
		}
	})

	return stop
}

func startFileUploader(bucket string, jobID int64, filePath, key, detail string) func() {
	var once sync.Once
	done := make(chan struct{})
	stopped := make(chan struct{})
	stop := func() {
		once.Do(func() { close(done) })
		<-stopped
	}

	upload := func() {
		info, err := os.Stat(filePath)
		if err != nil || info.Size() == 0 {
			return
		}

		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, "rclone", "copyto",
			filePath, fmt.Sprintf("r2:%s/%s", bucket, key))
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err = cmd.Run()
		cancel()
		if err != nil {
			errDetail := fmt.Sprintf("%s size=%d", detail, info.Size())
			if s := strings.TrimSpace(stderr.String()); s != "" {
				errDetail += " stderr=" + s
			}
			oplog.Log(oplog.OpR2Copy, oplog.WithJobID(jobID),
				oplog.WithDetail(errDetail), oplog.WithError(err),
				oplog.WithDuration(time.Since(start)))
			return
		}
		oplog.Log(oplog.OpR2Copy, oplog.WithJobID(jobID),
			oplog.WithDetail(detail), oplog.WithDuration(time.Since(start)))
	}

	fatalAgentGo("file-uploader", func() {
		defer close(stopped)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				upload()
				return
			case <-ticker.C:
				upload()
			}
		}
	})

	return stop
}

// syncString is a mutex-protected string for sharing phase state between goroutines.
type syncString struct {
	mu  sync.Mutex
	val string
}

func (s *syncString) Set(v string) {
	s.mu.Lock()
	s.val = v
	s.mu.Unlock()
}

func (s *syncString) Get() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.val
}

// readLogTail reads the last maxBytes of a file. Returns empty string on error.
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
