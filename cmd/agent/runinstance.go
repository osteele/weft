package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/osteele/weft/internal/artifacts"
	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/progress"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2resolve"
	"github.com/osteele/weft/internal/r2upload"
	"github.com/osteele/weft/internal/runner"
)

// runInstance implements the cloud instance worker subcommand.
// Usage: weft-agent run-instance --r2-bucket=BUCKET --instance-id=ID [--log-dir=/tmp/weft-logs] [--max-time=2h] [--grace-period=15m]
func runInstance(args []string) {
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
	oplog.Log(oplog.OpAgentStart, oplog.WithDetailf("run-instance instance=%s", instanceID))

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
	for _, job := range manifest.Jobs {
		registerSourceMounts(job.SourceMounts)
	}

	// Export env vars from manifest
	for k, v := range manifest.Env {
		os.Setenv(k, v)
	}

	// Apply drain tunables from manifest. Zero fields fall back to the
	// r2upload package defaults (already set as the var initializers).
	applyDrainSettings(manifest.Drain)

	// R2 key for instance phase tracking
	phaseKey := r2keys.InstancePhase(instanceIDInt)

	// Track current phase for heartbeat reporting
	var currentPhase syncString
	currentPhase.Set("starting")

	// Wire instance identity into the upload-drain layer so that persistent
	// R2 stalls can trigger an instance self-destruct without each upload
	// call site needing to thread it through manually.
	setUploadStallSelfDestructContext(instanceIDInt, manifest.SelfDestructCmd, currentPhase.Get)
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
			detail := fmt.Sprintf("runInstance main panic: %v", r)
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
	publication := &publicationState{}
	forceHeartbeat, stopHeartbeat := startHeartbeatReporter(r2Bucket, instanceIDInt, diskPath, currentPhase.Get, publication)
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
	runHFCacheHygieneAtStartup(manifest.Jobs)

	// Verify the provider actually allocated the container disk we asked
	// for. Vast.ai (and others) can silently cap the request on a
	// multi-tenant host. Failing fast here avoids running for hours and
	// hitting ENOSPC mid-job.
	if checkDiskCap(r2Bucket, instanceIDInt, manifest.RequestedDiskGB, diskPath, manifest.SelfDestructCmd) {
		return
	}

	// Verify the host's NVIDIA driver is new enough for the wheels the
	// campaign's jobs will load. RunPod doesn't expose driver_version at
	// offer search, so we may have landed on an old-driver host even when
	// MinDriverVersion was set. Vast offers are filtered at search time but
	// the field can be stale, so we run the check there too. Failing fast
	// here saves the ~3-5 minute setup before vLLM/torch hits the runtime
	// driver check.
	if checkDriverVersion(r2Bucket, instanceIDInt, manifest.RequiredDriverMajor, manifest.SelfDestructCmd) {
		return
	}

	// Hedge probe: launched with no jobs. Idle and poll R2 for jobs
	// migrated by a later reuse pass — without this branch the agent
	// would exit immediately and self-destruct.
	if len(manifest.Jobs) == 0 {
		onPhase("ready")
		probeSig := make(chan os.Signal, 1)
		signal.Notify(probeSig, syscall.SIGINT, syscall.SIGTERM)
		defer signal.Stop(probeSig)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-probeSig:
				return
			case <-ticker.C:
				newJobs := checkForNewJobs(r2Bucket, instanceIDInt, onPhase)
				if len(newJobs) == 0 {
					continue
				}
				_ = runJobSequence(newJobs, jobSequenceConfig{
					R2Bucket:                 r2Bucket,
					InstanceID:               instanceIDInt,
					PhaseKey:                 phaseKey,
					LogDir:                   logDir,
					DiskPath:                 diskPath,
					MaxTime:                  maxTime,
					StartTime:                startTime,
					OnPhase:                  onPhase,
					SkipWorkdirDeletion:      manifest.SkipWorkdirDeletion || skipWorkdirDeletion,
					GPUWarmup:                manifest.GPUWarmup,
					SelfDestructCmd:          manifest.SelfDestructCmd,
					PublicationState:         publication,
					PublicationWorkers:       manifest.Publication.Workers,
					PublicationQueueCapacity: manifest.Publication.QueueCapacity,
					PublicationMaxRetained:   manifest.Publication.MaxRetainedBytes,
				})
				return
			}
		}
	}

	seqResult := runJobSequence(manifest.Jobs, jobSequenceConfig{
		R2Bucket:                 r2Bucket,
		InstanceID:               instanceIDInt,
		PhaseKey:                 phaseKey,
		LogDir:                   logDir,
		DiskPath:                 diskPath,
		MaxTime:                  maxTime,
		StartTime:                startTime,
		OnPhase:                  onPhase,
		SkipWorkdirDeletion:      manifest.SkipWorkdirDeletion || skipWorkdirDeletion,
		GPUWarmup:                manifest.GPUWarmup,
		CostPerHourCents:         manifest.CostPerHourCents,
		Provider:                 manifest.Provider,
		InstanceType:             manifest.InstanceType,
		Resumed:                  resumed,
		SelfDestructCmd:          manifest.SelfDestructCmd,
		PublicationState:         publication,
		PublicationWorkers:       manifest.Publication.Workers,
		PublicationQueueCapacity: manifest.Publication.QueueCapacity,
		PublicationMaxRetained:   manifest.Publication.MaxRetainedBytes,
	})
	anyFailed = seqResult.AnyFailed

	// Grace period or self-destruct
	if anyFailed && !seqResult.AnyInfraFailed && gracePeriod > 0 {
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
		terminalStatus, terminationReason := terminalOutcomeForSequence(seqResult)
		selfDestruct(selfDestructOpts{
			Bucket: r2Bucket, InstanceID: instanceID, SelfDestructCmd: manifest.SelfDestructCmd,
			TerminalStatus: terminalStatus, TerminationReason: terminationReason,
			Phase: currentPhase.Get(), CompletionManifest: seqResult.CompletionManifest,
		})
	}
}

func terminalOutcomeForSequence(seqResult jobSequenceResult) (string, string) {
	if seqResult.AnyInfraFailed {
		return db.LaunchStatusFailed, db.TerminationReasonInfraFailure
	}
	if seqResult.AnyFailed {
		return db.LaunchStatusFailed, db.TerminationReasonJobFailure
	}
	if seqResult.AnyCanceled && seqResult.StartedJobCount == 0 {
		return db.LaunchStatusCancelled, db.TerminationReasonCancelled
	}
	return db.LaunchStatusCompleted, db.TerminationReasonCompleted
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
// Best-effort is intentional: sync-side phase reconciliation is DB-driven
// for the running state, so dropped phase PUTs only affect transient sub-state display.
func writePhase(r2Bucket, phaseKey, phase string) {
	go r2Put(r2Bucket, phaseKey, phase)
}

// recordPhase routes one phase transition through every phase surface: the
// in-process phase setter (if any), the ops log, and the R2 phase marker
// (best-effort, see writePhase). jobID is 0 for instance-level phases
// ("grace", "destroying") that carry no job suffix; "verb:jobID" phases
// pass the job so the ops log entry is attributable.
func recordPhase(r2Bucket, phaseKey, phase string, jobID int64, setPhase func(string)) {
	if setPhase != nil {
		setPhase(phase)
	}
	oplog.Log(oplog.OpPhaseTransition, oplog.WithJobID(jobID), oplog.WithDetail(phase))
	writePhase(r2Bucket, phaseKey, phase)
}

// phaseCallback returns an OnPhase callback that writes phase markers to R2.
func phaseCallback(r2Bucket, phaseKey string, jobID int64, setPhase func(string)) func(string) {
	return func(phase string) {
		recordPhase(r2Bucket, phaseKey, fmt.Sprintf("%s:%d", phase, jobID), jobID, setPhase)
	}
}

// uploadOutputDirs uploads convention-based output directories to R2.
//
// sinceUnix is the attempt's start time; files modified before it are not
// uploaded under this job's prefix. Without the window, the walk would also
// upload a prior same-workdir job's leftovers under this job's keys (spec:
// invariant Attribution in specs/job-lifecycle.allium). Pass 0 to disable
// (restaged outputs, unknown start).
func uploadOutputDirs(bucket string, jobID, runID int64, workDir string, sinceUnix int64, outputDirs []string) runner.OutputUploadResult {
	startedAt := time.Now()
	var result runner.OutputUploadResult
	var attempted int
	var failed int
	var totalDuration time.Duration
	workDir = runner.ExpandTilde(workDir)
	windowStart := runner.AttemptOutputThreshold(sinceUnix)

	for _, dir := range config.EffectiveOutputDirs(outputDirs) {
		dir = strings.TrimRight(dir, "/")
		dirPath := filepath.Join(workDir, dir)
		info, err := os.Stat(dirPath)
		if err != nil || !info.IsDir() {
			continue
		}
		fileCount, bytes, measured := measureUploadTreeSince(dirPath, windowStart)
		if measured && fileCount == 0 {
			// Everything in the dir predates this attempt; nothing to upload.
			continue
		}
		opts := drainOptionsFromConfig()
		attempted++
		if !measured {
			bytes = bytesForMaxDrain(opts)
		}
		start := time.Now()
		retryCount := 0
		opts.Source = dirPath + "/"
		opts.DestRemote = "r2:" + bucket + "/" + r2keys.JobAttemptOutputDir(jobID, runID, dir)
		opts.Command = "copy"
		opts.Extra = outputUploadRcloneArgs(windowStart)
		opts.TotalBytes = bytes
		drainResult := drainAndMarkWithRetry(context.Background(), bucket, drainTarget{
			JobID: jobID, RunID: runID, Label: fmt.Sprintf("output dir=%s", dir),
		}, opts)
		if drainResult.Status != r2upload.StatusOK {
			duration := time.Since(start)
			totalDuration += duration
			result.Dirs = append(result.Dirs, runner.OutputDirUpload{
				Dir:        dir,
				Status:     runner.UploadStatusFailed,
				Error:      fmt.Sprintf("%s: %s", drainResult.Status, drainResult.Reason),
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
// job script and uploads each declared artifact file (plus the manifest
// itself). Workdir-relative declarations beneath convention output dirs use
// their canonical outputs/ object; other declarations use artifacts/files/.
var uploadArtifactObject = rcloneUploadWithRetry

func uploadArtifactManifestEntries(bucket string, jobID, runID int64, workDir string, outputDirs []string) runner.OutputUploadResult {
	startedAt := time.Now()
	manifestPath := runner.ExpandTilde(artifacts.RemoteManifestPath(jobID))
	manifest, err := artifacts.ReadManifestFile(manifestPath, jobID)
	if err != nil && !errors.Is(err, artifacts.ErrManifestMissing) {
		// An unreadable manifest fails the whole upload
		// (spec: PerArtifactUploadOutcomeRecorded).
		result := runner.OutputUploadResult{
			Dirs: []runner.OutputDirUpload{{
				Dir:    manifestPath,
				Status: runner.UploadStatusFailed,
				Error:  fmt.Sprintf("read artifact manifest: %v", err),
			}},
		}
		finalizeUploadResult(&result, 1, 1, startedAt, 0)
		return result
	}
	if len(manifest.Artifacts) == 0 {
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
	outputsPrefix := r2keys.JobAttemptOutputsPrefix(jobID, runID)

	var result runner.OutputUploadResult
	var attempted, failed int
	var totalDuration time.Duration
	type uploadEntry struct {
		spec                 artifacts.ArtifactSpec
		src, dest, rcloneCmd string
		remotePath           string
		fileCount            int
		bytes                int64
		info                 fs.FileInfo
		statErr              error
		coveredBy            int
		status, errStr       string
		duration             time.Duration
	}
	entries := make([]uploadEntry, 0, len(manifest.Artifacts))
	for _, spec := range manifest.Artifacts {
		if strings.TrimSpace(spec.Path) == "" {
			continue
		}
		attempted++
		remotePath := artifacts.ResolveRemotePath(root, spec.Path)
		remotePath = runner.ExpandTilde(remotePath)
		info, statErr := os.Stat(remotePath)
		entry := uploadEntry{spec: spec, remotePath: remotePath, info: info, statErr: statErr, coveredBy: -1}
		if statErr != nil {
			entries = append(entries, entry)
			continue
		}
		localRel := filepath.ToSlash(artifacts.LocalRelativePath(spec.Path))
		dest := filesPrefix + localRel
		if outputRel, ok := r2resolve.ConventionOutputRelPath(manifest, spec.Path, outputDirs); ok {
			dest = outputsPrefix + outputRel
		}
		if info.IsDir() {
			var measured bool
			entry.fileCount, entry.bytes, measured = measureUploadTree(remotePath)
			if !measured {
				entry.bytes = -1
			}
			entry.src, entry.dest, entry.rcloneCmd = remotePath+"/", dest+"/", "copy"
		} else {
			entry.fileCount, entry.bytes = 1, info.Size()
			entry.src, entry.dest, entry.rcloneCmd = remotePath, dest, "copyto"
		}
		entries = append(entries, entry)
	}

	// Collapse lexical duplicates and declarations covered by an ancestor
	// directory. All declarations remain in the manifest and receive a logical
	// outcome below; only the payload owner invokes rclone.
	for i := range entries {
		if entries[i].statErr != nil {
			continue
		}
		for j := range entries {
			if i == j || entries[j].statErr != nil {
				continue
			}
			sameObject := filepath.Clean(entries[i].remotePath) == filepath.Clean(entries[j].remotePath) &&
				strings.TrimRight(entries[i].dest, "/") == strings.TrimRight(entries[j].dest, "/")
			parentObject := entries[j].info.IsDir() && pathWithin(entries[j].remotePath, entries[i].remotePath) &&
				keyWithin(entries[j].dest, entries[i].dest)
			if (sameObject && j < i) || parentObject {
				if entries[i].coveredBy < 0 || len(entries[j].dest) < len(entries[entries[i].coveredBy].dest) {
					entries[i].coveredBy = j
				}
			}
		}
	}

	for i := range entries {
		entry := &entries[i]
		if entry.statErr != nil {
			fmt.Fprintf(os.Stderr, "artifact %q for job %d: %v\n", entry.spec.Path, jobID, entry.statErr)
			entry.status, entry.errStr = runner.UploadStatusFailed, entry.statErr.Error()
			failed++
			continue
		}
		if entry.coveredBy >= 0 {
			continue
		}
		start := time.Now()
		uploadErr := uploadArtifactObject(bucket, entry.src, entry.dest, entry.rcloneCmd, jobID, runID, entry.spec.Path, entry.bytes)
		entry.duration = time.Since(start)
		totalDuration += entry.duration
		entry.status = runner.UploadStatusOK
		if uploadErr != nil {
			failed++
			entry.status = runner.UploadStatusFailed
			entry.errStr = uploadErr.Error()
		}
		result.FileCount += entry.fileCount
		result.Bytes += entry.bytes
	}
	for i := range entries {
		entry := &entries[i]
		if entry.coveredBy >= 0 {
			owner := &entries[entry.coveredBy]
			entry.status, entry.errStr, entry.duration = owner.status, owner.errStr, owner.duration
			if entry.status == runner.UploadStatusFailed {
				failed++
			}
		}
		result.Dirs = append(result.Dirs, runner.OutputDirUpload{
			Dir: entry.spec.Path, Status: entry.status, Error: entry.errStr,
			FileCount: entry.fileCount, Bytes: entry.bytes, DurationMS: entry.duration.Milliseconds(),
		})
	}

	finalizeUploadResult(&result, attempted, failed, startedAt, totalDuration)
	return result
}

func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func keyWithin(parent, child string) bool {
	parent = strings.TrimRight(parent, "/")
	child = strings.TrimRight(child, "/")
	return child != parent && strings.HasPrefix(child, parent+"/")
}

// outputUploadRcloneArgs builds the rclone filter args for a convention-output
// upload. A non-zero windowStart becomes an absolute --max-age bound so rclone
// skips files modified before the attempt began.
func outputUploadRcloneArgs(windowStart time.Time) []string {
	args := []string{"--update"}
	if !windowStart.IsZero() {
		args = append(args, "--max-age", windowStart.UTC().Format(time.RFC3339))
	}
	return args
}

// rcloneUploadWithRetry runs an rclone command with retries on transient
// failures, using the shared stall-watchdog + ceiling drain gate so the
// upload-failure marker lands on stall/ceiling kills.
func rcloneUploadWithRetry(bucket, src, r2Key, rcloneCmd string, jobID, runID int64, label string, sizeBytes int64) error {
	opts := drainOptionsFromConfig()
	opts.Source = src
	opts.DestRemote = "r2:" + bucket + "/" + r2Key
	opts.Command = rcloneCmd
	if rcloneCmd == "copy" || rcloneCmd == "copyto" {
		opts.Extra = []string{"--update"}
	}
	if sizeBytes < 0 {
		sizeBytes = bytesForMaxDrain(opts)
	}
	opts.TotalBytes = sizeBytes
	result := drainAndMarkWithRetry(context.Background(), bucket, drainTarget{
		JobID: jobID, RunID: runID, Label: label,
	}, opts)
	if result.Status != r2upload.StatusOK {
		return fmt.Errorf("%s: %s", result.Status, result.Reason)
	}
	return nil
}

// finalizeUploadResult sets status and timing fields on an OutputUploadResult.
func finalizeUploadResult(result *runner.OutputUploadResult, attempted, failed int, startedAt time.Time, totalDuration time.Duration) {
	result.Status = uploadStatusFor(attempted, failed)
	result.DurationMS = totalDuration.Milliseconds()
	if attempted > 0 {
		result.StartedAtUnix = startedAt.Unix()
		result.CompletedAtUnix = time.Now().Unix()
	}
}

func uploadStatusFor(attempted, failed int) string {
	switch {
	case attempted == 0 || failed == 0:
		return runner.UploadStatusOK
	case failed == attempted:
		return runner.UploadStatusFailed
	default:
		return runner.UploadStatusPartial
	}
}

// mergeUploadResults folds src into dst: per-entry outcomes, counters, timing,
// and a combined status re-derived from the merged entries. Both producers
// append exactly one Dirs entry per attempted upload
// (spec: PerArtifactUploadOutcomeRecorded).
func mergeUploadResults(dst, src *runner.OutputUploadResult) {
	dst.Dirs = append(dst.Dirs, src.Dirs...)
	dst.FileCount += src.FileCount
	dst.Bytes += src.Bytes
	dst.RetryCount += src.RetryCount
	dst.DurationMS += src.DurationMS
	if src.StartedAtUnix != 0 && (dst.StartedAtUnix == 0 || src.StartedAtUnix < dst.StartedAtUnix) {
		dst.StartedAtUnix = src.StartedAtUnix
	}
	if src.CompletedAtUnix > dst.CompletedAtUnix {
		dst.CompletedAtUnix = src.CompletedAtUnix
	}
	failed := 0
	for _, d := range dst.Dirs {
		if d.Status == runner.UploadStatusFailed {
			failed++
		}
	}
	dst.Status = uploadStatusFor(len(dst.Dirs), failed)
}

func measureUploadTree(root string) (files int, bytes int64, ok bool) {
	return measureUploadTreeSince(root, time.Time{})
}

// measureUploadTreeSince counts files and bytes under root, skipping files
// modified before since (zero = no filter), matching the --max-age bound the
// upload itself applies.
func measureUploadTreeSince(root string, since time.Time) (files int, bytes int64, ok bool) {
	ok = true
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		if !since.IsZero() && info.ModTime().Before(since) {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	}); err != nil {
		return 0, 0, false
	}
	return files, bytes, true
}

func bytesForMaxDrain(opts r2upload.Options) int64 {
	if opts.MaxDrain <= 0 {
		opts.MaxDrain = r2upload.DefaultMaxDrain
	}
	if opts.Baseline <= 0 {
		opts.Baseline = r2upload.DefaultBaseline
	}
	if opts.FloorThroughput <= 0 {
		opts.FloorThroughput = r2upload.DefaultFloorThroughput
	}
	if opts.MaxDrain <= opts.Baseline || opts.FloorThroughput <= 0 {
		return 1 << 60
	}
	bytes := int64(opts.MaxDrain-opts.Baseline) * opts.FloorThroughput / int64(time.Second)
	if bytes <= 0 {
		return 1 << 60
	}
	return bytes
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
func runJobWithProgress(r2Bucket string, jobID, runID, instanceID int64, logDir string, cfg runner.SingleJobConfig, outputsSinceUnix int64) (runner.ExitInfo, error) {
	logPath := filepath.Join(logDir, fmt.Sprintf("%d.log", jobID))
	stopProgress := startProgressReporter(r2Bucket, jobID, runID, logPath)
	stopLogs := startLogUploader(r2Bucket, jobID, runID, logPath)
	stopOutputs := startOutputUploader(r2Bucket, jobID, runID, cfg.WorkingDir, outputsSinceUnix, cfg.Job.OutputDirs)
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
					// Graceful escalation: SIGTERM now, SIGKILL after the
					// grace window. This gives the job a chance to flush
					// checkpoints and lets the bash exit-capture trap write
					// the status file. KillProcessGroupWithGrace also writes
					// the kill-reason file.
					runner.KillProcessGroupWithGrace(pgid, runner.DefaultKillGrace,
						runner.NewJobPaths(logDir, jobID), runner.KillReasonUserKill)
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
func startHeartbeatReporter(r2Bucket string, instanceID int64, diskPath string, getPhase func() string, publication *publicationState) (force func(), stop func()) {
	heartbeatKey := r2keys.InstanceHeartbeat(instanceID)
	emit := func() {
		sample := collectHeartbeatWithPublication(getPhase(), diskPath, publication)
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
// sinceUnix windows the convention-output walk to this attempt and outputDirs
// carries the job's configured dirs (see uploadOutputDirs).
func startOutputUploader(bucket string, jobID, runID int64, workDir string, sinceUnix int64, outputDirs []string) func() {
	var once sync.Once
	done := make(chan struct{})
	stopped := make(chan struct{})
	stop := func() {
		once.Do(func() { close(done) })
		<-stopped
	}

	workDir = runner.ExpandTilde(workDir)
	upload := func() {
		_ = uploadArtifactManifestEntries(bucket, jobID, runID, workDir, outputDirs)
		_ = uploadOutputDirs(bucket, jobID, runID, workDir, sinceUnix, outputDirs)
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

	fatalAgentGo("file-uploader", func() {
		defer close(stopped)
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				uploadLiveFileSnapshot(bucket, jobID, filePath, key, detail)
				return
			case <-ticker.C:
				uploadLiveFileSnapshot(bucket, jobID, filePath, key, detail)
			}
		}
	})

	return stop
}

func uploadLiveFileSnapshot(bucket string, jobID int64, filePath, key, detail string) {
	data, err := os.ReadFile(filePath)
	if err != nil || len(data) == 0 {
		return
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	cmd := exec.CommandContext(ctx, "rclone", "rcat", fmt.Sprintf("r2:%s/%s", bucket, key))
	cmd.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	cancel()
	if err != nil {
		errDetail := fmt.Sprintf("%s size=%d", detail, len(data))
		if s := strings.TrimSpace(stderr.String()); s != "" {
			errDetail += " stderr=" + s
		}
		oplog.Log(oplog.OpR2Copy, oplog.WithJobID(jobID),
			oplog.WithDetail(errDetail), oplog.WithError(err),
			oplog.WithDuration(time.Since(start)))
		return
	}
	oplog.Log(oplog.OpR2Copy, oplog.WithJobID(jobID),
		oplog.WithDetail(fmt.Sprintf("%s size=%d snapshot=true", detail, len(data))),
		oplog.WithDuration(time.Since(start)))
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
