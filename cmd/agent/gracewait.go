package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/r2upload"
	"github.com/osteele/weft/internal/retry"
	"github.com/osteele/weft/internal/runner"
)

// graceStatus is written to R2 as grace/<instanceID>/status.
type graceStatus struct {
	State      string  `json:"state"`    // "waiting", "running", "completed"
	Deadline   string  `json:"deadline"` // RFC3339
	FailedJobs []int64 `json:"failed_jobs"`
}

// graceWaitConfig holds parameters for the grace-wait loop.
type graceWaitConfig struct {
	InstanceID          string
	R2Bucket            string
	Timeout             time.Duration
	SelfDestructCmd     string
	LogDir              string
	SkipWorkdirDeletion bool
}

// parseGraceWaitArgs parses CLI args into a graceWaitConfig for the grace-wait subcommand.
func parseGraceWaitArgs(args []string) graceWaitConfig {
	var cfg graceWaitConfig

	for _, arg := range args {
		switch {
		case hasPrefix(arg, "--instance-id="):
			cfg.InstanceID = arg[len("--instance-id="):]
		case hasPrefix(arg, "--r2-bucket="):
			cfg.R2Bucket = arg[len("--r2-bucket="):]
		case hasPrefix(arg, "--timeout="):
			val := arg[len("--timeout="):]
			var err error
			cfg.Timeout, err = time.ParseDuration(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid --timeout: %s\n", val)
				os.Exit(1)
			}
		case hasPrefix(arg, "--self-destruct-cmd="):
			cfg.SelfDestructCmd = arg[len("--self-destruct-cmd="):]
		case hasPrefix(arg, "--log-dir="):
			cfg.LogDir = arg[len("--log-dir="):]
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", arg)
			os.Exit(1)
		}
	}

	if cfg.InstanceID == "" || cfg.R2Bucket == "" || cfg.Timeout == 0 {
		fmt.Fprintln(os.Stderr, "required: --instance-id, --r2-bucket, --timeout")
		os.Exit(1)
	}
	if cfg.SelfDestructCmd == "" {
		fmt.Fprintln(os.Stderr, "required: --self-destruct-cmd")
		os.Exit(1)
	}

	return cfg
}

// graceWait implements the grace-wait subcommand (CLI entry point).
func graceWait(args []string) {
	graceWaitLoop(parseGraceWaitArgs(args))
}

// graceWaitLoop polls R2 for control messages and keeps the instance alive for resubmission.
func graceWaitLoop(cfg graceWaitConfig) {
	instanceID := cfg.InstanceID
	r2Bucket := cfg.R2Bucket
	selfDestructCmd := cfg.SelfDestructCmd
	logDir := cfg.LogDir
	instanceIDInt, err := strconv.ParseInt(instanceID, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid instance ID %q: %v\n", instanceID, err)
		os.Exit(1)
	}

	oplogPath := filepath.Join(logDir, agentOpslogFile)
	if err := oplog.Init(oplogPath, 0); err != nil {
		fmt.Fprintf(os.Stderr, "warning: init opslog: %v\n", err)
	}
	defer oplog.Close()
	oplog.Log(oplog.OpAgentStart, oplog.WithDetailf("grace-wait instance=%s", instanceID))

	deadline := time.Now().Add(cfg.Timeout)
	prefix := controlplane.GracePrefix(instanceIDInt)
	phaseKey := controlplane.InstancePhase(instanceIDInt)

	writePhase(r2Bucket, phaseKey, "grace")

	// Write initial status
	writeGraceStatus(r2Bucket, prefix, graceStatus{
		State:    "waiting",
		Deadline: deadline.Format(time.RFC3339),
	})

	fmt.Printf("Grace period started. Deadline: %s\n", deadline.Format(time.RFC3339))
	fmt.Printf("Polling R2 at %s/ every 10s\n", prefix)

	pollInterval := 10 * time.Second
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		<-ticker.C

		// Check deadline
		if time.Now().After(deadline) {
			fmt.Println("Grace period expired. Self-destructing.")
			uploadOpslog(r2Bucket, instanceIDInt, logDir)
			selfDestruct(selfDestructOpts{
				Bucket: r2Bucket, InstanceID: instanceID, SelfDestructCmd: selfDestructCmd,
				TerminalStatus: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonJobFailure,
				Phase: "destroying", CompletionManifest: collectCompletionManifest(logDir, nil),
			})
			return
		}

		// Check for release requests.
		releaseRequested, err := hasGraceReleaseRequest(r2Bucket, instanceIDInt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "poll grace release: %v\n", err)
		} else if releaseRequested {
			fmt.Println("Release signal received. Self-destructing.")
			uploadOpslog(r2Bucket, instanceIDInt, logDir)
			selfDestruct(selfDestructOpts{
				Bucket: r2Bucket, InstanceID: instanceID, SelfDestructCmd: selfDestructCmd,
				TerminalStatus: db.LaunchStatusFailed, TerminationReason: db.TerminationReasonJobFailure,
				Phase: "destroying", CompletionManifest: collectCompletionManifest(logDir, nil),
			})
			return
		}

		// Apply any queued extend requests.
		updatedDeadline, err := applyGraceExtendRequests(r2Bucket, instanceIDInt, deadline)
		if err != nil {
			fmt.Fprintf(os.Stderr, "poll grace extend: %v\n", err)
		} else if !updatedDeadline.Equal(deadline) {
			deadline = updatedDeadline
			fmt.Printf("Grace period extended. New deadline: %s\n", deadline.Format(time.RFC3339))
			writeGraceStatus(r2Bucket, prefix, graceStatus{
				State:    "waiting",
				Deadline: deadline.Format(time.RFC3339),
			})
		}

		// Check for resubmitted jobs.
		jobs, err := drainGraceJobRequests(r2Bucket, instanceIDInt, func(phase string) {
			writePhase(r2Bucket, phaseKey, phase)
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "poll grace jobs: %v\n", err)
			continue
		}
		if len(jobs) == 0 {
			continue
		}

		// Update status to running
		writeGraceStatus(r2Bucket, prefix, graceStatus{
			State:    "running",
			Deadline: deadline.Format(time.RFC3339),
		})

		// Run resubmitted jobs using the shared job loop. Time spent
		// running these jobs must not count against the grace deadline —
		// otherwise a resubmitted job that runs longer than the
		// remaining grace causes self-destruction the instant it
		// finishes (and leaves grace_deadline < grace_started_at on
		// the coordinator side once the next grace entry refreshes
		// grace_started_at against a stale R2 deadline).
		jobStart := time.Now()
		seqResult := runJobSequence(jobs, jobSequenceConfig{
			R2Bucket:            r2Bucket,
			InstanceID:          instanceIDInt,
			PhaseKey:            phaseKey,
			LogDir:              logDir,
			StartTime:           jobStart,
			SkipWorkdirDeletion: cfg.SkipWorkdirDeletion,
		})
		failedJobs := seqResult.FailedJobs
		deadline = extendGraceDeadlineForJobs(deadline, jobStart, time.Now())

		if len(failedJobs) == 0 {
			// All jobs succeeded — self-destruct
			fmt.Println("All resubmitted jobs succeeded. Self-destructing.")
			uploadOpslog(r2Bucket, instanceIDInt, logDir)
			selfDestruct(selfDestructOpts{
				Bucket: r2Bucket, InstanceID: instanceID, SelfDestructCmd: selfDestructCmd,
				TerminalStatus: db.LaunchStatusCompleted, TerminationReason: db.TerminationReasonCompleted,
				Phase: "destroying", CompletionManifest: seqResult.CompletionManifest,
			})
			return
		}

		// Some jobs failed — resume waiting. Write the refreshed
		// status (with extended deadline) BEFORE writing phase=grace
		// so the coordinator can't observe phase=grace with a stale
		// deadline still in R2.
		fmt.Printf("Some jobs failed. Resuming grace period until %s\n", deadline.Format(time.RFC3339))
		writeGraceStatus(r2Bucket, prefix, graceStatus{
			State:      "waiting",
			Deadline:   deadline.Format(time.RFC3339),
			FailedJobs: failedJobs,
		})
		writePhase(r2Bucket, phaseKey, "grace")
	}
}

func writeGraceStatus(bucket, prefix string, status graceStatus) {
	data, _ := json.Marshal(status)
	if err := r2Put(bucket, prefix+"/status", string(data)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write grace status: %v\n", err)
	}
}

// extendGraceDeadlineForJobs returns deadline shifted forward by the elapsed
// time spent running resubmitted jobs (jobEnd - jobStart). Resubmitted-job
// execution must not consume the post-failure recovery window: a user who
// resubmits a 20-minute job into a 15-minute grace would otherwise lose the
// instance the instant the job finishes, even when no further action is
// pending. Returns deadline unchanged if jobEnd is not after jobStart
// (clock skew or zero-duration runs).
func extendGraceDeadlineForJobs(deadline, jobStart, jobEnd time.Time) time.Time {
	if !jobEnd.After(jobStart) {
		return deadline
	}
	return deadline.Add(jobEnd.Sub(jobStart))
}

func uploadJobResults(bucket string, jobID, runID int64, logDir string) runner.UploadSummary {
	summary := runner.UploadSummary{
		Status:        "ok",
		StartedAtUnix: time.Now().Unix(),
	}
	summary.FileCount, summary.Bytes = measureUploadTree(logDir)

	start := time.Now()
	opts := drainOptionsFromConfig()
	opts.Source = logDir + "/"
	opts.DestRemote = "r2:" + bucket + "/" + r2keys.JobAttemptResultsPrefix(jobID, runID)
	opts.Command = "copy"
	opts.TotalBytes = summary.Bytes

	result := drainAndMark(context.Background(), bucket, drainTarget{
		JobID: jobID, RunID: runID, Label: fmt.Sprintf("results dir=%s", logDir),
	}, opts)
	summary.DurationMS = time.Since(start).Milliseconds()
	summary.CompletedAtUnix = time.Now().Unix()
	if result.Status != r2upload.StatusOK {
		summary.Status = "failed"
		summary.Error = fmt.Sprintf("%s: %s", result.Status, result.Reason)
	}

	// The .complete marker is written synchronously in runJobSequence
	// (before background uploads start) so the coordinator sees jobs
	// finish in order. This function only handles result uploads.
	return summary
}

type selfDestructOpts struct {
	Bucket             string
	InstanceID         string
	SelfDestructCmd    string
	TerminalStatus     string
	TerminationReason  string
	Phase              string
	JobID              int64
	CompletionManifest *runner.InstanceCompletionManifest
}

func selfDestruct(opts selfDestructOpts) {
	instanceIDInt, _ := strconv.ParseInt(opts.InstanceID, 10, 64)
	phaseKey := controlplane.InstancePhase(instanceIDInt)
	marker := &instanceintent.Marker{
		TerminalStatus:    opts.TerminalStatus,
		TerminationReason: opts.TerminationReason,
		Phase:             opts.Phase,
		JobID:             opts.JobID,
		State:             instanceintent.StateOpen,
		RequestedAtUnix:   time.Now().Unix(),
	}

	// Write completion marker: JSON manifest if available, bare "0" as fallback
	completionPayload := "0"
	if opts.CompletionManifest != nil {
		if data, err := json.Marshal(opts.CompletionManifest); err == nil {
			completionPayload = string(data)
		}
	}
	r2Put(opts.Bucket, controlplane.CampaignComplete(instanceIDInt), completionPayload)
	writePhase(opts.Bucket, phaseKey, "destroying")
	writeTerminationIntent(opts.Bucket, instanceIDInt, *marker)

	// Clean up grace keys
	prefix := controlplane.GracePrefix(instanceIDInt)
	r2Delete(opts.Bucket, prefix+"/status")

	executeSelfDestruct(opts.Bucket, instanceIDInt, opts.SelfDestructCmd, marker)
}

func writeTerminationIntent(bucket string, instanceID int64, marker instanceintent.Marker) {
	data, err := json.Marshal(marker)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal termination intent: %v\n", err)
		return
	}
	if err := r2Put(bucket, controlplane.InstanceTerminationIntent(instanceID), string(data)); err != nil {
		fmt.Fprintf(os.Stderr, "write termination intent: %v\n", err)
	}
}

func executeSelfDestruct(bucket string, instanceID int64, selfDestructCmd string, marker *instanceintent.Marker) {
	fmt.Printf("Executing self-destruct: %s\n", selfDestructCmd)
	if marker != nil && marker.DestroyStartedAtUnix == 0 {
		marker.State = instanceintent.StateDestroying
		marker.DestroyStartedAtUnix = time.Now().Unix()
		writeTerminationIntent(bucket, instanceID, *marker)
	}
	attempt := 0
	err := retry.Do(context.Background(), retry.FixedAttempts(3, 5*time.Second), func() error {
		attempt++
		if marker != nil {
			marker.DestroyAttempts = attempt
			marker.LastAttemptAtUnix = time.Now().Unix()
			marker.LastError = ""
			writeTerminationIntent(bucket, instanceID, *marker)
		}
		cmd := exec.Command("bash", "-c", selfDestructCmd)
		var stderr bytes.Buffer
		cmd.Stdout = os.Stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			if marker != nil {
				marker.LastError = err.Error()
				if s := strings.TrimSpace(stderr.String()); s != "" {
					marker.LastError = marker.LastError + ": " + s
				}
				writeTerminationIntent(bucket, instanceID, *marker)
			}
			return fmt.Errorf("self-destruct attempt %d failed: %v (stderr: %s)", attempt, err, stderr.String())
		}
		return nil
	}, retry.WithOnRetry(func(n int, err error, delay time.Duration) {
		fmt.Fprintf(os.Stderr, "%v; retrying in %s\n", err, delay)
	}))
	if err != nil {
		if marker != nil {
			marker.State = instanceintent.StateFailed
			writeTerminationIntent(bucket, instanceID, *marker)
		}
		fmt.Fprintf(os.Stderr, "WARNING: self-destruct failed after 3 attempts, instance may still be running\n")
		return
	}
	if marker != nil {
		marker.State = instanceintent.StateSucceeded
		marker.DestroySucceededAtUnix = time.Now().Unix()
		marker.LastError = ""
		writeTerminationIntent(bucket, instanceID, *marker)
	}
	fmt.Printf("Self-destruct succeeded on attempt %d\n", attempt)
}
