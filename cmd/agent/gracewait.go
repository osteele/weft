package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/ops"
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

	deadline := time.Now().Add(cfg.Timeout)
	prefix := fmt.Sprintf("grace/%s", instanceID)

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
			selfDestruct(r2Bucket, instanceID, selfDestructCmd)
			return
		}

		// Check for release signal
		if releaseVal, _ := r2Get(r2Bucket, prefix+"/release"); releaseVal != "" {
			fmt.Println("Release signal received. Self-destructing.")
			r2Delete(r2Bucket, prefix+"/release")
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

		// Run resubmitted jobs
		var failedJobs []int64
		for _, job := range payload.Jobs {
			fmt.Printf("Running resubmitted job %d: %s\n", job.ID, job.Command)

			// Write .started marker to R2
			r2Put(r2Bucket, fmt.Sprintf("jobs/%d/.started", job.ID), fmt.Sprintf("%d", time.Now().Unix()))

			workDir := job.Dir
			if workDir == "" {
				workDir = workspace
			}

			cfg := runner.SingleJobConfig{
				JobID: job.ID,
				Job: ops.CommandJob{
					Cmd: job.Command,
				},
				LogDir:     logDir,
				WorkingDir: workDir,
			}

			ei, err := runner.RunSingleJob(cfg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "run-job %d failed: %v\n", job.ID, err)
				failedJobs = append(failedJobs, job.ID)
				continue
			}

			uploadOutputDirs(r2Bucket, job.ID, workDir)
			uploadJobResults(r2Bucket, job.ID, logDir)

			if ei.ExitCode != 0 {
				fmt.Printf("Job %d failed (exit %d)\n", job.ID, ei.ExitCode)
				failedJobs = append(failedJobs, job.ID)
			} else {
				fmt.Printf("Job %d completed successfully\n", job.ID)
			}
		}

		if len(failedJobs) == 0 {
			// All jobs succeeded — self-destruct
			fmt.Println("All resubmitted jobs succeeded. Self-destructing.")
			selfDestruct(r2Bucket, instanceID, selfDestructCmd)
			return
		}

		// Some jobs failed — resume waiting
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
	rcloneCmd := exec.CommandContext(copyCtx, "rclone", "copy", logDir+"/", fmt.Sprintf("r2:%s/jobs/%d/results/", bucket, jobID))
	rcloneCmd.Stderr = os.Stderr
	_ = rcloneCmd.Run()
	copyCancel()

	// Write completion marker
	completeCtx, completeCancel := context.WithTimeout(context.Background(), r2Timeout)
	completeCmd := exec.CommandContext(completeCtx, "rclone", "rcat", fmt.Sprintf("r2:%s/jobs/%d/.complete", bucket, jobID))
	completeCmd.Stdin = strings.NewReader("done")
	_ = completeCmd.Run()
	completeCancel()
}

func selfDestruct(bucket, instanceID, selfDestructCmd string) {
	// Write completion marker
	r2Put(bucket, fmt.Sprintf("campaigns/%s/.complete", instanceID), "0")

	// Clean up grace keys
	prefix := fmt.Sprintf("grace/%s", instanceID)
	r2Delete(bucket, prefix+"/status")

	// Execute self-destruct
	fmt.Printf("Executing self-destruct: %s\n", selfDestructCmd)
	cmd := exec.Command("bash", "-c", selfDestructCmd)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}
