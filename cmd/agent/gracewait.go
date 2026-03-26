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

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/instanceintent"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2keys"
	"github.com/osteele/weft/internal/runner"
)

// graceStatus is written to R2 as grace/<instanceID>/status.
type graceStatus struct {
	State      string  `json:"state"`    // "waiting", "running", "completed"
	Deadline   string  `json:"deadline"` // RFC3339
	FailedJobs []int64 `json:"failed_jobs"`
}

// graceJobsPayload is read from R2 as grace/<instanceID>/jobs.json.
type graceJobsPayload struct {
	Jobs []cloud.AgentJob `json:"jobs"`
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
	prefix := r2keys.GracePrefix(instanceIDInt)
	phaseKey := r2keys.InstancePhase(instanceIDInt)

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
				TerminalStatus: db.CloudInstanceStatusFailed, TerminationReason: db.TerminationReasonJobFailure,
				Phase: "destroying",
			})
			return
		}

		// Check for release signal
		if releaseVal, _ := r2Get(r2Bucket, prefix+"/release"); releaseVal != "" {
			fmt.Println("Release signal received. Self-destructing.")
			r2Delete(r2Bucket, prefix+"/release")
			uploadOpslog(r2Bucket, instanceIDInt, logDir)
			selfDestruct(selfDestructOpts{
				Bucket: r2Bucket, InstanceID: instanceID, SelfDestructCmd: selfDestructCmd,
				TerminalStatus: db.CloudInstanceStatusFailed, TerminationReason: db.TerminationReasonJobFailure,
				Phase: "destroying",
			})
			return
		}

		// Check for extend signal
		extendVal, _ := r2Get(r2Bucket, prefix+"/extend")
		if extendVal != "" {
			dur, err := time.ParseDuration(extendVal)
			if err == nil {
				deadline = time.Now().Add(dur)
				fmt.Printf("Grace period extended. New deadline: %s\n", deadline.Format(time.RFC3339))
				writeGraceStatus(r2Bucket, prefix, graceStatus{
					State:    "waiting",
					Deadline: deadline.Format(time.RFC3339),
				})
			}
			r2Delete(r2Bucket, prefix+"/extend")
		}

		// Check for resubmitted jobs
		jobsJSON, _ := r2Get(r2Bucket, prefix+"/jobs.json")
		if jobsJSON == "" {
			continue
		}

		var payload graceJobsPayload
		if err := json.Unmarshal([]byte(jobsJSON), &payload); err != nil {
			fmt.Fprintf(os.Stderr, "failed to parse jobs.json: %v\n", err)
			r2Delete(r2Bucket, prefix+"/jobs.json")
			continue
		}

		if len(payload.Jobs) == 0 {
			r2Delete(r2Bucket, prefix+"/jobs.json")
			continue
		}

		// Acknowledge receipt
		r2Delete(r2Bucket, prefix+"/jobs.json")
		r2Put(r2Bucket, prefix+"/ack", fmt.Sprintf("%d", time.Now().Unix()))

		// Update status to running
		writeGraceStatus(r2Bucket, prefix, graceStatus{
			State:    "running",
			Deadline: deadline.Format(time.RFC3339),
		})

		// Run resubmitted jobs using the shared job loop
		seqResult := runJobSequence(payload.Jobs, jobSequenceConfig{
			R2Bucket:            r2Bucket,
			InstanceID:          instanceIDInt,
			PhaseKey:            phaseKey,
			LogDir:              logDir,
			StartTime:           time.Now(),
			SkipWorkdirDeletion: cfg.SkipWorkdirDeletion,
		})
		failedJobs := seqResult.FailedJobs

		if len(failedJobs) == 0 {
			// All jobs succeeded — self-destruct
			fmt.Println("All resubmitted jobs succeeded. Self-destructing.")
			uploadOpslog(r2Bucket, instanceIDInt, logDir)
			cm := collectCompletionManifest(logDir, payload.Jobs)
			selfDestruct(selfDestructOpts{
				Bucket: r2Bucket, InstanceID: instanceID, SelfDestructCmd: selfDestructCmd,
				TerminalStatus: db.CloudInstanceStatusCompleted, TerminationReason: db.TerminationReasonCompleted,
				Phase: "destroying", CompletionManifest: cm,
			})
			return
		}

		// Some jobs failed — resume waiting
		writePhase(r2Bucket, phaseKey, "grace")
		fmt.Printf("Some jobs failed. Resuming grace period until %s\n", deadline.Format(time.RFC3339))
		writeGraceStatus(r2Bucket, prefix, graceStatus{
			State:      "waiting",
			Deadline:   deadline.Format(time.RFC3339),
			FailedJobs: failedJobs,
		})
	}
}

func writeGraceStatus(bucket, prefix string, status graceStatus) {
	data, _ := json.Marshal(status)
	if err := r2Put(bucket, prefix+"/status", string(data)); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write grace status: %v\n", err)
	}
}

func uploadJobResults(bucket string, jobID, runID int64, logDir string) runner.UploadSummary {
	summary := runner.UploadSummary{
		Status:        "ok",
		StartedAtUnix: time.Now().Unix(),
	}
	summary.FileCount, summary.Bytes = measureUploadTree(logDir)
	// Upload per-job results
	copyCtx, copyCancel := context.WithTimeout(context.Background(), 60*time.Second)
	rcloneCmd := exec.CommandContext(copyCtx, "rclone", "copy", logDir+"/", "r2:"+bucket+"/"+r2keys.JobAttemptResultsPrefix(jobID, runID))
	rcloneCmd.Stderr = os.Stderr
	start := time.Now()
	copyOK := false
	if err := rcloneCmd.Run(); err != nil {
		summary.Status = "failed"
		summary.Error = err.Error()
		fmt.Fprintf(os.Stderr, "upload results for job %d: %v\n", jobID, err)
		oplog.Log(oplog.OpR2Copy, oplog.WithJobID(jobID),
			oplog.WithDetailf("results dir=%s", logDir), oplog.WithError(err),
			oplog.WithDuration(time.Since(start)))
	} else {
		copyOK = true
	}
	summary.DurationMS = time.Since(start).Milliseconds()
	summary.CompletedAtUnix = time.Now().Unix()
	copyCancel()

	if copyOK {
		cleanupLiveLogUpload(bucket, jobID, runID)
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
	phaseKey := r2keys.InstancePhase(instanceIDInt)
	marker := &instanceintent.Marker{
		TerminalStatus:    opts.TerminalStatus,
		TerminationReason: opts.TerminationReason,
		Phase:             opts.Phase,
		JobID:             opts.JobID,
		RequestedAtUnix:   time.Now().Unix(),
	}

	// Write completion marker: JSON manifest if available, bare "0" as fallback
	completionPayload := "0"
	if opts.CompletionManifest != nil {
		if data, err := json.Marshal(opts.CompletionManifest); err == nil {
			completionPayload = string(data)
		}
	}
	r2Put(opts.Bucket, r2keys.CampaignComplete(instanceIDInt), completionPayload)
	writePhase(opts.Bucket, phaseKey, "destroying")
	writeTerminationIntent(opts.Bucket, instanceIDInt, *marker)

	// Clean up grace keys
	prefix := r2keys.GracePrefix(instanceIDInt)
	r2Delete(opts.Bucket, prefix+"/status")

	executeSelfDestruct(opts.Bucket, instanceIDInt, opts.SelfDestructCmd, marker)
}

func writeTerminationIntent(bucket string, instanceID int64, marker instanceintent.Marker) {
	data, err := json.Marshal(marker)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal termination intent: %v\n", err)
		return
	}
	if err := r2Put(bucket, r2keys.InstanceTerminationIntent(instanceID), string(data)); err != nil {
		fmt.Fprintf(os.Stderr, "write termination intent: %v\n", err)
	}
}

func executeSelfDestruct(bucket string, instanceID int64, selfDestructCmd string, marker *instanceintent.Marker) {
	// Execute self-destruct with retries
	fmt.Printf("Executing self-destruct: %s\n", selfDestructCmd)
	if marker != nil && marker.DestroyStartedAtUnix == 0 {
		marker.DestroyStartedAtUnix = time.Now().Unix()
		writeTerminationIntent(bucket, instanceID, *marker)
	}
	for attempt := 1; attempt <= 3; attempt++ {
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
			fmt.Fprintf(os.Stderr, "self-destruct attempt %d failed: %v (stderr: %s)\n", attempt, err, stderr.String())
		} else {
			if marker != nil {
				marker.DestroySucceededAtUnix = time.Now().Unix()
				marker.LastError = ""
				writeTerminationIntent(bucket, instanceID, *marker)
			}
			fmt.Printf("Self-destruct succeeded on attempt %d\n", attempt)
			return
		}
		time.Sleep(5 * time.Second)
	}
	fmt.Fprintf(os.Stderr, "WARNING: self-destruct failed after 3 attempts, instance may still be running\n")
}
