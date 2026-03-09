package cloud

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// defaultOutputDirs mirrors config.DefaultOutputDirs for use in the wrapper script.
// Cannot import config directly due to import cycle (config -> cloud).
var defaultOutputDirs = []string{"output", "outputs"}

// AgentJob describes a job for the agent-based wrapper, serialized as JSON for stdin.
type AgentJob struct {
	ID      int64  `json:"id"`
	Command string `json:"cmd"`
	Dir     string `json:"dir,omitempty"`
}

// WrapperOpts configures optional aspects of the generated wrapper script.
type WrapperOpts struct {
	EnvVars            map[string]string // Extra environment variables to export
	MaxTimeSeconds     int               // Instance time budget; remaining time is passed per-job as --max-time
	DBInstanceID       int64             // DB instance ID for R2 completion marker (campaigns/<id>/.complete)
	GracePeriodSeconds int               // Grace period after job failure (0 = disabled, self-destruct immediately)
}

// GenerateAgentWrapper produces a thin bash script that delegates job execution
// to the weft-agent binary. The agent handles process management, telemetry
// sampling, failure detection, and completion records. The wrapper handles only
// R2 upload and self-destruct.
func GenerateAgentWrapper(client Client, jobs []AgentJob, r2Bucket string, providerInstanceID string, opts WrapperOpts) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -o pipefail\n\n")

	// Ensure uv and rclone are in PATH (installed by onstart to ~/.local/bin and /usr/bin)
	b.WriteString("export PATH=\"$HOME/.local/bin:$PATH\"\n")

	// Export extra environment variables (e.g. HF_TOKEN for gated model downloads)
	for _, k := range slices.Sorted(maps.Keys(opts.EnvVars)) {
		b.WriteString(fmt.Sprintf("export %s=%q\n", k, opts.EnvVars[k]))
	}
	b.WriteString("\n")

	b.WriteString(fmt.Sprintf("R2_BUCKET=%q\n", r2Bucket))
	if opts.DBInstanceID > 0 {
		b.WriteString(fmt.Sprintf("INSTANCE_ID=%d\n", opts.DBInstanceID))
	}
	b.WriteString("LOG_DIR=\"/tmp/weft-logs\"\n")
	b.WriteString("mkdir -p \"$LOG_DIR\"\n")

	// Instance-level time budget tracking
	if opts.MaxTimeSeconds > 0 {
		b.WriteString(fmt.Sprintf("\nINSTANCE_START=$(date +%%s)\n"))
		b.WriteString(fmt.Sprintf("MAX_SECONDS=%d\n", opts.MaxTimeSeconds))
	}
	b.WriteString("\n")

	// Track failed jobs for grace period
	if opts.GracePeriodSeconds > 0 {
		b.WriteString(fmt.Sprintf("GRACE_SECONDS=%d\n", opts.GracePeriodSeconds))
	}
	b.WriteString("ANY_FAILED=0\n\n")

	// Wrap jobs in a function so we can 'return' to skip remaining jobs on timeout
	b.WriteString("run_jobs() {\n")

	for _, job := range jobs {
		b.WriteString(fmt.Sprintf("# --- Job %d ---\n", job.ID))
		b.WriteString(fmt.Sprintf("JOB_ID=%d\n", job.ID))

		// Marshal job to JSON for stdin
		jobData, _ := json.Marshal(job)
		jobJSON := string(jobData)

		jobWorkDir := client.WorkspacePath()
		if job.Dir != "" {
			jobWorkDir = job.Dir
		}
		workingDirFlag := fmt.Sprintf(" --working-dir=%s", jobWorkDir)

		// Compute remaining time budget for this job
		if opts.MaxTimeSeconds > 0 {
			b.WriteString("ELAPSED=$(( $(date +%s) - INSTANCE_START ))\n")
			b.WriteString("REMAINING=$(( MAX_SECONDS - ELAPSED ))\n")
			b.WriteString("if [ $REMAINING -le 0 ]; then\n")
			b.WriteString("  echo \"Instance time budget exhausted, skipping remaining jobs\"\n")
			b.WriteString("  return\n")
			b.WriteString("fi\n")
		}

		maxTimeFlag := ""
		if opts.MaxTimeSeconds > 0 {
			maxTimeFlag = " --max-time=${REMAINING}s"
		}

		// Write .started marker with timestamp to R2 (background, don't block job start)
		b.WriteString("echo \"$(date +%s)\" | rclone rcat \"r2:$R2_BUCKET/jobs/$JOB_ID/.started\" &\n")

		b.WriteString(fmt.Sprintf("echo '%s' | weft-agent run-job --job-id=$JOB_ID --log-dir=$LOG_DIR%s%s\n", jobJSON, workingDirFlag, maxTimeFlag))

		// Capture exit code for grace period tracking
		b.WriteString("JOB_EXIT=$?\n")
		b.WriteString("if [ $JOB_EXIT -ne 0 ]; then ANY_FAILED=1; fi\n")
		b.WriteString("\n")

		// Upload job output files (convention-based output directories)
		b.WriteString(fmt.Sprintf("JOB_WORKDIR=%q\n", jobWorkDir))
		for _, outDir := range defaultOutputDirs {
			b.WriteString(fmt.Sprintf("if [ -d \"$JOB_WORKDIR/%s\" ]; then\n", outDir))
			b.WriteString(fmt.Sprintf("  rclone copy \"$JOB_WORKDIR/%s/\" \"r2:$R2_BUCKET/jobs/$JOB_ID/outputs/%s/\" 2>/dev/null\n", outDir, outDir))
			b.WriteString("fi\n")
		}

		// Upload per-job results
		b.WriteString("# Upload per-job results\n")
		b.WriteString("rclone copy $LOG_DIR/ \"r2:$R2_BUCKET/jobs/$JOB_ID/results/\" 2>/dev/null\n")
		b.WriteString("echo \"done\" | rclone rcat \"r2:$R2_BUCKET/jobs/$JOB_ID/.complete\"\n")

		// Promote uv manifest to content-addressable R2 key
		b.WriteString("# Promote uv manifest to content-addressable key (idempotent)\n")
		b.WriteString("if [ -f \"$LOG_DIR/uv-manifest.json\" ]; then\n")
		b.WriteString("  UV_META=$(python3 -c \"import json,sys; m=json.load(sys.stdin); print(m['lockfile_hash'], m['platform'])\" < \"$LOG_DIR/uv-manifest.json\")\n")
		b.WriteString("  LOCK_HASH=${UV_META%% *}\n")
		b.WriteString("  PLATFORM=${UV_META##* }\n")
		b.WriteString("  rclone copyto \"$LOG_DIR/uv-manifest.json\" \"r2:$R2_BUCKET/uv-manifests/$LOCK_HASH/$PLATFORM.json\" 2>/dev/null\n")
		b.WriteString("fi\n")

		b.WriteString("rm -f $LOG_DIR/*\n\n")
	}

	b.WriteString("}\nrun_jobs\n\n")

	// Grace period: if any job failed and grace is configured, enter grace-wait
	if opts.GracePeriodSeconds > 0 {
		selfDestructCmd := client.SelfDestructCmd(providerInstanceID)
		b.WriteString("# Grace period: keep instance alive after failure for fix-and-resubmit\n")
		b.WriteString("if [ $ANY_FAILED -eq 1 ] && [ ${GRACE_SECONDS:-0} -gt 0 ]; then\n")
		b.WriteString("  echo \"Jobs failed. Entering grace period (${GRACE_SECONDS}s).\"\n")
		b.WriteString(fmt.Sprintf("  weft-agent grace-wait --instance-id=$INSTANCE_ID --r2-bucket=$R2_BUCKET --timeout=${GRACE_SECONDS}s --self-destruct-cmd=%q --log-dir=$LOG_DIR --workspace=%s\n",
			selfDestructCmd, client.WorkspacePath()))
		b.WriteString("  exit 0\n")
		b.WriteString("fi\n\n")
	}

	// Instance completion marker + self-destruct (always runs, even after timeout)
	b.WriteString("# Instance completion marker + self-destruct\n")
	b.WriteString("echo \"0\" | rclone rcat \"r2:$R2_BUCKET/campaigns/$INSTANCE_ID/.complete\"\n")
	b.WriteString(client.SelfDestructCmd(providerInstanceID))
	b.WriteString("\n")

	return b.String()
}
