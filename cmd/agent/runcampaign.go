package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/cloudlog"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/progress"
	"github.com/osteele/weft/internal/r2keys"
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

	startTime := time.Now()
	anyFailed := false

	// Upload agent startup timestamp to R2 for boot timing analysis
	go func() {
		payload, _ := json.Marshal(map[string]int64{"agent_start_unix": startTime.Unix()})
		_ = r2Put(r2Bucket, r2keys.InstanceAgentStartup(instanceIDInt), string(payload))
	}()

	// Start heartbeat reporter (writes host metrics to R2 every 30s)
	stopHeartbeat := startHeartbeatReporter(r2Bucket, instanceIDInt, diskPath, currentPhase.Get)
	defer stopHeartbeat()
	stopOpslogReporter := startOpslogReporter(r2Bucket, instanceIDInt, logDir)
	defer stopOpslogReporter()
	stopDiskMonitor := startDiskMonitor(r2Bucket, instanceIDInt, diskPath, logDir, phaseKey, manifest.SelfDestructCmd, currentPhase.Get, currentPhase.Set)
	defer stopDiskMonitor()

	seqResult := runJobSequence(manifest.Jobs, jobSequenceConfig{
		R2Bucket:            r2Bucket,
		InstanceID:          instanceIDInt,
		PhaseKey:            phaseKey,
		LogDir:              logDir,
		MaxTime:             maxTime,
		StartTime:           startTime,
		OnPhase:             currentPhase.Set,
		SkipWorkdirDeletion: manifest.SkipWorkdirDeletion || skipWorkdirDeletion,
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

// checkForNewJobs reads the grace/jobs.json R2 key and returns any newly
// submitted jobs. Deletes the key after reading (acknowledge receipt).
// This enables running instances to pick up jobs submitted via campaign reuse.
func checkForNewJobs(r2Bucket string, instanceID int64) []cloud.AgentJob {
	jobsJSON, _ := r2Get(r2Bucket, r2keys.GraceJobs(instanceID))
	if jobsJSON == "" {
		return nil
	}

	var payload graceJobsPayload
	if err := json.Unmarshal([]byte(jobsJSON), &payload); err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse between-job jobs.json: %v\n", err)
		r2Delete(r2Bucket, r2keys.GraceJobs(instanceID))
		return nil
	}

	// Acknowledge and delete
	r2Delete(r2Bucket, r2keys.GraceJobs(instanceID))
	r2Put(r2Bucket, r2keys.GraceAck(instanceID), fmt.Sprintf("%d", time.Now().Unix()))

	return payload.Jobs
}

// writePhase writes a phase marker to R2 in a background goroutine.
// Best-effort: errors are logged but do not block the caller.
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
		start := time.Now()
		var lastErr error
		retryCount := 0
		for attempt := 1; attempt <= 3; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			cmd := exec.CommandContext(ctx, "rclone", "copy",
				dirPath+"/",
				"r2:"+bucket+"/"+r2keys.JobAttemptOutputDir(jobID, runID, dir),
			)
			cmd.Stderr = os.Stderr
			err := cmd.Run()
			cancel()
			if err == nil {
				duration := time.Since(start)
				oplog.Log(oplog.OpR2Copy, oplog.WithJobID(jobID),
					oplog.WithDetailf("output dir=%s", dir),
					oplog.WithDuration(duration))
				lastErr = nil
				break
			}
			lastErr = err
			retryCount++
			if attempt < 3 {
				backoff := 5 * time.Second
				if attempt == 2 {
					backoff = 10 * time.Second
				}
				fmt.Fprintf(os.Stderr, "upload outputs %s for job %d (attempt %d/3): %v; retrying in %s\n",
					dir, jobID, attempt, err, backoff)
				time.Sleep(backoff)
			}
		}
		if lastErr != nil {
			duration := time.Since(start)
			totalDuration += duration
			fmt.Fprintf(os.Stderr, "upload outputs %s for job %d failed: %v\n", dir, jobID, lastErr)
			oplog.Log(oplog.OpR2Copy, oplog.WithJobID(jobID),
				oplog.WithDetailf("output dir=%s", dir), oplog.WithError(lastErr),
				oplog.WithDuration(duration))
			result.Dirs = append(result.Dirs, runner.OutputDirUpload{
				Dir:        dir,
				Status:     "failed",
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
		result.FileCount += fileCount
		result.Bytes += bytes
		result.RetryCount += retryCount
		result.Dirs = append(result.Dirs, runner.OutputDirUpload{
			Dir:        dir,
			Status:     "ok",
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
		return runner.OutputUploadResult{Status: "ok"}
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
		if info.IsDir() {
			fileCount, bytes := measureUploadTree(remotePath)
			r2Dest := filesPrefix + artifacts.LocalRelativePath(spec.Path) + "/"
			start := time.Now()
			uploadErr := rcloneUploadWithRetry(bucket, remotePath+"/", r2Dest, "copy", jobID, spec.Path)
			duration := time.Since(start)
			totalDuration += duration
			status := "ok"
			var errStr string
			if uploadErr != nil {
				failed++
				status = "failed"
				errStr = uploadErr.Error()
			}
			result.Dirs = append(result.Dirs, runner.OutputDirUpload{
				Dir: spec.Path, Status: status, Error: errStr,
				FileCount: fileCount, Bytes: bytes, DurationMS: duration.Milliseconds(),
			})
			result.FileCount += fileCount
			result.Bytes += bytes
		} else {
			r2Key := filesPrefix + artifacts.LocalRelativePath(spec.Path)
			start := time.Now()
			uploadErr := rcloneUploadWithRetry(bucket, remotePath, r2Key, "copyto", jobID, spec.Path)
			duration := time.Since(start)
			totalDuration += duration
			if uploadErr != nil {
				failed++
			}
			result.FileCount++
			result.Bytes += info.Size()
		}
	}

	finalizeUploadResult(&result, attempted, failed, startedAt, totalDuration)
	return result
}

// rcloneUploadWithRetry runs an rclone command with up to 3 attempts and backoff.
func rcloneUploadWithRetry(bucket, src, r2Key, rcloneCmd string, jobID int64, label string) error {
	for attempt := 1; attempt <= 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		cmd := exec.CommandContext(ctx, "rclone", rcloneCmd, src, "r2:"+bucket+"/"+r2Key)
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		cancel()
		if err == nil {
			return nil
		}
		if attempt < 3 {
			backoff := 5 * time.Second
			if attempt == 2 {
				backoff = 10 * time.Second
			}
			fmt.Fprintf(os.Stderr, "upload %s for job %d (attempt %d/3): %v; retrying in %s\n",
				label, jobID, attempt, err, backoff)
			time.Sleep(backoff)
		} else {
			fmt.Fprintf(os.Stderr, "upload %s for job %d failed: %v\n", label, jobID, err)
			return err
		}
	}
	return nil
}

// finalizeUploadResult sets status and timing fields on an OutputUploadResult.
func finalizeUploadResult(result *runner.OutputUploadResult, attempted, failed int, startedAt time.Time, totalDuration time.Duration) {
	switch {
	case attempted == 0 || failed == 0:
		result.Status = "ok"
	case failed == attempted:
		result.Status = "failed"
	default:
		result.Status = "partial"
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
func runJobWithProgress(r2Bucket string, jobID, runID int64, logDir string, cfg runner.SingleJobConfig) (runner.ExitInfo, error) {
	logPath := filepath.Join(logDir, fmt.Sprintf("%d.log", jobID))
	stopProgress := startProgressReporter(r2Bucket, jobID, runID, logPath)
	stopLogs := startLogUploader(r2Bucket, jobID, runID, logPath)
	defer func() {
		stopProgress()
		stopLogs()
		r2Delete(r2Bucket, r2keys.JobAttemptProgress(jobID, runID))
	}()
	return runner.RunSingleJob(cfg)
}

// startProgressReporter starts a goroutine that periodically reads the job log,
// parses progress lines, and writes the percent to R2. Returns a stop function.
func startProgressReporter(r2Bucket string, jobID, runID int64, logPath string) func() {
	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }

	go func() {
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
				prog := progress.FindLastProgress(tail)
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
	}()

	return stop
}

// startHeartbeatReporter starts a goroutine that writes host-level metrics
// to R2 every 30 seconds. getPhase returns the current instance phase string.
// Returns a stop function.
func startHeartbeatReporter(r2Bucket string, instanceID int64, diskPath string, getPhase func() string) func() {
	var once sync.Once
	done := make(chan struct{})
	stop := func() { once.Do(func() { close(done) }) }

	heartbeatKey := r2keys.InstanceHeartbeat(instanceID)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				sample := collectHeartbeat(getPhase(), diskPath)
				data, err := json.Marshal(sample)
				if err != nil {
					continue
				}
				r2Put(r2Bucket, heartbeatKey, string(data))
			}
		}
	}()

	return stop
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

	go func() {
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
	}()

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

	go func() {
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
	}()

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

func cleanupLiveLogUpload(bucket string, jobID, runID int64) {
	if err := r2Delete(bucket, cloudlog.ManifestKeyForRun(jobID, runID)); err != nil {
		fmt.Fprintf(os.Stderr, "delete live log manifest for job %d: %v\n", jobID, err)
	}
	if err := r2Delete(bucket, cloudlog.PrefixForRun(jobID, runID)); err != nil {
		fmt.Fprintf(os.Stderr, "delete live log parts for job %d: %v\n", jobID, err)
	}
}
