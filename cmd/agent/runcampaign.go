package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/ops"
	"github.com/osteele/weft/internal/runner"
)

// runCampaign implements the "run-campaign" subcommand.
// Usage: weft-agent run-campaign --r2-bucket=BUCKET --instance-id=ID [--workspace=/workspace/] [--log-dir=/tmp/weft-logs] [--max-time=2h] [--grace-period=15m]
func runCampaign(args []string) {
	var r2Bucket string
	var instanceID string
	var workspace string
	var logDir string
	var maxTime time.Duration
	var gracePeriod time.Duration

	for _, arg := range args {
		switch {
		case hasPrefix(arg, "--r2-bucket="):
			r2Bucket = arg[len("--r2-bucket="):]
		case hasPrefix(arg, "--instance-id="):
			instanceID = arg[len("--instance-id="):]
		case hasPrefix(arg, "--workspace="):
			workspace = arg[len("--workspace="):]
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
		default:
			fmt.Fprintf(os.Stderr, "unknown flag: %s\n", arg)
			os.Exit(1)
		}
	}

	if r2Bucket == "" || instanceID == "" {
		fmt.Fprintln(os.Stderr, "required: --r2-bucket, --instance-id")
		os.Exit(1)
	}
	if workspace == "" {
		workspace = "/workspace/"
	}
	if logDir == "" {
		logDir = "/tmp/weft-logs"
	}
	_ = os.MkdirAll(logDir, 0o755)

	// Fetch manifest from R2
	manifestKey := fmt.Sprintf("campaigns/%s/manifest.json", instanceID)
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
	phaseKey := fmt.Sprintf("instance/%s/phase", instanceID)

	startTime := time.Now()
	anyFailed := false

	for _, job := range manifest.Jobs {
		// Check time budget
		if maxTime > 0 {
			elapsed := time.Since(startTime)
			remaining := maxTime - elapsed
			if remaining <= 0 {
				fmt.Println("Instance time budget exhausted, skipping remaining jobs")
				break
			}
		}

		fmt.Printf("--- Job %d ---\n", job.ID)

		// Write .started marker to R2
		r2Put(r2Bucket, fmt.Sprintf("jobs/%d/.started", job.ID), fmt.Sprintf("%d", time.Now().Unix()))

		workDir := job.Dir
		if workDir == "" {
			workDir = workspace
		}

		// Compute per-job max time from remaining budget
		var jobMaxTime time.Duration
		if maxTime > 0 {
			jobMaxTime = maxTime - time.Since(startTime)
		}

		cfg := runner.SingleJobConfig{
			JobID: job.ID,
			Job: ops.CommandJob{
				Cmd: job.Command,
			},
			LogDir:     logDir,
			WorkingDir: workDir,
			MaxTime:    jobMaxTime,
			OnPhase:    phaseCallback(r2Bucket, phaseKey, job.ID),
		}

		ei, err := runner.RunSingleJob(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "run-job %d failed: %v\n", job.ID, err)
			anyFailed = true
		} else if ei.ExitCode != 0 {
			fmt.Printf("Job %d failed (exit %d)\n", job.ID, ei.ExitCode)
			anyFailed = true
		} else {
			fmt.Printf("Job %d completed successfully\n", job.ID)
		}

		writePhase(r2Bucket, phaseKey, fmt.Sprintf("uploading:%d", job.ID))

		// Upload output directories
		uploadOutputDirs(r2Bucket, job.ID, workDir)

		// Upload per-job results
		uploadJobResults(r2Bucket, job.ID, logDir)

		// Promote uv manifest
		promoteUVManifest(r2Bucket, logDir)

		// Clean log dir for next job
		cleanLogDir(logDir)
	}

	// Grace period or self-destruct
	if anyFailed && gracePeriod > 0 {
		fmt.Printf("Jobs failed. Entering grace period (%s).\n", gracePeriod)
		graceWaitLoop(graceWaitConfig{
			InstanceID:      instanceID,
			R2Bucket:        r2Bucket,
			Timeout:         gracePeriod,
			SelfDestructCmd: manifest.SelfDestructCmd,
			LogDir:          logDir,
			Workspace:       workspace,
		})
	} else {
		selfDestruct(r2Bucket, instanceID, manifest.SelfDestructCmd)
	}
}

// writePhase writes a phase marker to R2 in a background goroutine.
// Best-effort: errors are logged but do not block the caller.
func writePhase(r2Bucket, phaseKey, phase string) {
	go r2Put(r2Bucket, phaseKey, phase)
}

// phaseCallback returns an OnPhase callback that writes phase markers to R2.
func phaseCallback(r2Bucket, phaseKey string, jobID int64) func(string) {
	return func(phase string) {
		writePhase(r2Bucket, phaseKey, fmt.Sprintf("%s:%d", phase, jobID))
	}
}

// uploadOutputDirs uploads convention-based output directories to R2.
func uploadOutputDirs(bucket string, jobID int64, workDir string) {
	for _, dir := range config.DefaultOutputDirs {
		dir = strings.TrimRight(dir, "/")
		dirPath := filepath.Join(workDir, dir)
		info, err := os.Stat(dirPath)
		if err != nil || !info.IsDir() {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		cmd := exec.CommandContext(ctx, "rclone", "copy",
			dirPath+"/",
			fmt.Sprintf("r2:%s/jobs/%d/outputs/%s/", bucket, jobID, dir),
		)
		cmd.Stderr = os.Stderr
		_ = cmd.Run()
		cancel()
	}
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
	key := fmt.Sprintf("uv-manifests/%s/%s.json", m.LockfileHash, m.Platform)
	r2Put(bucket, key, string(data))
}

// cleanLogDir removes all contents of the log directory to prepare for the next job.
func cleanLogDir(logDir string) {
	os.RemoveAll(logDir)
	os.MkdirAll(logDir, 0o755)
}
