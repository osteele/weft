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
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/oplog"
	"github.com/osteele/weft/internal/r2keys"
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
	InstanceID      string
	R2Bucket        string
	Timeout         time.Duration
	SelfDestructCmd string
	LogDir          string
	Workspace       string
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
		case hasPrefix(arg, "--workspace="):
			cfg.Workspace = arg[len("--workspace="):]
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
	workspace := cfg.Workspace

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
			selfDestruct(r2Bucket, instanceID, selfDestructCmd)
			return
		}

		// Check for release signal
		if releaseVal, _ := r2Get(r2Bucket, prefix+"/release"); releaseVal != "" {
			fmt.Println("Release signal received. Self-destructing.")
			r2Delete(r2Bucket, prefix+"/release")
			uploadOpslog(r2Bucket, instanceIDInt, logDir)
			selfDestruct(r2Bucket, instanceID, selfDestructCmd)
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
			R2Bucket:   r2Bucket,
			InstanceID: instanceIDInt,
			PhaseKey:   phaseKey,
			LogDir:     logDir,
			Workspace:  workspace,
			StartTime:  time.Now(),
		})
		failedJobs := seqResult.FailedJobs

		if len(failedJobs) == 0 {
			// All jobs succeeded — self-destruct
			fmt.Println("All resubmitted jobs succeeded. Self-destructing.")
			uploadOpslog(r2Bucket, instanceIDInt, logDir)
			selfDestruct(r2Bucket, instanceID, selfDestructCmd)
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

func uploadJobResults(bucket string, jobID int64, logDir string) {
	// Upload per-job results
	copyCtx, copyCancel := context.WithTimeout(context.Background(), 60*time.Second)
	rcloneCmd := exec.CommandContext(copyCtx, "rclone", "copy", logDir+"/", "r2:"+bucket+"/"+r2keys.JobResultsPrefix(jobID))
	rcloneCmd.Stderr = os.Stderr
	start := time.Now()
	copyOK := false
	if err := rcloneCmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "upload results for job %d: %v\n", jobID, err)
		oplog.Log(oplog.OpR2Copy, oplog.WithJobID(jobID),
			oplog.WithDetailf("results dir=%s", logDir), oplog.WithError(err),
			oplog.WithDuration(time.Since(start)))
	} else {
		copyOK = true
	}
	copyCancel()

	if copyOK {
		cleanupLiveLogUpload(bucket, jobID)
	}

	// Write completion marker
	if err := r2Put(bucket, r2keys.JobComplete(jobID), "done"); err != nil {
		fmt.Fprintf(os.Stderr, "write completion marker for job %d: %v\n", jobID, err)
	}
}

func selfDestruct(bucket, instanceID, selfDestructCmd string) {
	instanceIDInt, _ := strconv.ParseInt(instanceID, 10, 64)
	phaseKey := r2keys.InstancePhase(instanceIDInt)

	// Write completion marker and phase
	r2Put(bucket, r2keys.CampaignComplete(instanceIDInt), "0")
	writePhase(bucket, phaseKey, "destroying")

	// Clean up grace keys
	prefix := r2keys.GracePrefix(instanceIDInt)
	r2Delete(bucket, prefix+"/status")

	// Execute self-destruct with retries
	fmt.Printf("Executing self-destruct: %s\n", selfDestructCmd)
	for attempt := 1; attempt <= 3; attempt++ {
		cmd := exec.Command("bash", "-c", selfDestructCmd)
		var stderr bytes.Buffer
		cmd.Stdout = os.Stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "self-destruct attempt %d failed: %v (stderr: %s)\n", attempt, err, stderr.String())
		} else {
			fmt.Printf("Self-destruct succeeded on attempt %d\n", attempt)
			return
		}
		time.Sleep(5 * time.Second)
	}
	fmt.Fprintf(os.Stderr, "WARNING: self-destruct failed after 3 attempts, instance may still be running\n")
}
