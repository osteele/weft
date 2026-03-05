package vastai

import (
	"fmt"
	"strings"
)

// R2Config holds Cloudflare R2 credentials for instance-side uploads.
type R2Config struct {
	AccountID       string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
}

// GenerateWrapper produces a bash script that runs a job command, uploads
// results to R2, and self-destructs the Vast.ai instance.
//
// The script is deployed to /workspace/.weft-runner.sh and invoked via nohup.
// It expects CONTAINER_API_KEY and CONTAINER_ID env vars (injected by Vast.ai).
func GenerateWrapper(jobID int64, command string, r2Bucket string) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -o pipefail\n\n")

	b.WriteString(fmt.Sprintf("JOB_ID=\"%d\"\n", jobID))
	b.WriteString(fmt.Sprintf("R2_BUCKET=%q\n", r2Bucket))
	b.WriteString(`R2_PREFIX="jobs/$JOB_ID"`)
	b.WriteString("\n\n")

	// Phase timing: wrapper start
	b.WriteString("# Phase timing\n")
	b.WriteString("date -u +%s > /tmp/phase_start\n\n")

	// Setup phase (pre-job)
	b.WriteString("# --- Setup phase ---\n")
	b.WriteString("date -u +%s > /tmp/phase_setup_start\n")
	b.WriteString("cd /workspace\n")
	b.WriteString("date -u +%s > /tmp/phase_setup_end\n\n")

	// Job execution phase
	b.WriteString("# --- Job execution phase ---\n")
	b.WriteString("date -u +%s > /tmp/phase_run_start\n")

	// Start GPU monitor in background
	b.WriteString("# GPU monitoring\n")
	b.WriteString("(while true; do\n")
	b.WriteString("    nvidia-smi --query-gpu=utilization.gpu,memory.used --format=csv,noheader,nounits >> /tmp/gpu_monitor.csv 2>/dev/null\n")
	b.WriteString("    sleep 5\n")
	b.WriteString("done) &\n")
	b.WriteString("GPU_MONITOR_PID=$!\n\n")

	b.WriteString(fmt.Sprintf("{ %s ; } > /tmp/stdout.log 2> /tmp/stderr.log\n", command))
	b.WriteString("EXIT_CODE=$?\n")
	b.WriteString("kill $GPU_MONITOR_PID 2>/dev/null; wait $GPU_MONITOR_PID 2>/dev/null\n")
	b.WriteString("date -u +%s > /tmp/phase_run_end\n\n")

	// Capture metadata
	b.WriteString("# Capture metadata\n")
	b.WriteString(`echo "$EXIT_CODE" > /tmp/exit_code`)
	b.WriteString("\n")
	b.WriteString("date -u +%s > /tmp/end_time\n")
	b.WriteString(`echo "$CONTAINER_ID" > /tmp/instance_id`)
	b.WriteString("\n\n")

	// On failure: capture debug info
	b.WriteString("# On failure: capture debug info\n")
	b.WriteString("if [ $EXIT_CODE -ne 0 ]; then\n")
	b.WriteString("    mkdir -p /tmp/debug\n")
	b.WriteString("    cp /tmp/core.* /tmp/debug/ 2>/dev/null\n")
	b.WriteString("    dmesg | tail -100 > /tmp/debug/dmesg.log 2>/dev/null\n")
	b.WriteString("    nvidia-smi > /tmp/debug/nvidia-smi.log 2>/dev/null\n")
	b.WriteString("fi\n\n")

	// Assemble results
	b.WriteString("# Assemble results\n")
	b.WriteString("mkdir -p /tmp/results\n")
	b.WriteString("cp /tmp/stdout.log /tmp/stderr.log /tmp/exit_code /tmp/end_time /tmp/instance_id /tmp/results/\n")
	b.WriteString("cp /tmp/phase_* /tmp/results/\n")
	b.WriteString("cp /tmp/gpu_monitor.csv /tmp/results/ 2>/dev/null\n")
	b.WriteString("[ -d /tmp/debug ] && cp -r /tmp/debug /tmp/results/\n\n")

	// Record upload sizes
	b.WriteString("# Record upload sizes\n")
	b.WriteString("du -sb /tmp/results/ 2>/dev/null | cut -f1 > /tmp/results/upload_results_bytes\n")
	b.WriteString("du -sb /workspace/ 2>/dev/null | cut -f1 > /tmp/results/upload_workspace_bytes\n\n")

	// Upload phase
	b.WriteString("# --- Upload phase ---\n")
	b.WriteString("date -u +%s > /tmp/results/phase_upload_start\n")

	// Upload results to R2 with retry
	b.WriteString("# Upload results to R2 (retry up to 3 times)\n")
	b.WriteString("for attempt in 1 2 3; do\n")
	b.WriteString(`    rclone copy /tmp/results/ "r2:$R2_BUCKET/$R2_PREFIX/results/" && break`)
	b.WriteString("\n")
	b.WriteString("    sleep $((attempt * 5))\n")
	b.WriteString("done\n\n")

	// Upload workspace outputs
	b.WriteString("# Upload workspace outputs to R2\n")
	b.WriteString(`rclone copy /workspace/ "r2:$R2_BUCKET/$R2_PREFIX/workspace/" 2>/dev/null`)
	b.WriteString("\n")
	b.WriteString("date -u +%s > /tmp/phase_upload_end\n\n")

	// Write completion marker
	b.WriteString("# Write completion marker\n")
	b.WriteString(`echo "done" | rclone rcat "r2:$R2_BUCKET/$R2_PREFIX/.complete"`)
	b.WriteString("\n\n")

	// Self-destruct
	b.WriteString("# Self-destruct\n")
	b.WriteString(`vastai destroy instance "$CONTAINER_ID" --api-key "$CONTAINER_API_KEY" 2>/dev/null || true`)
	b.WriteString("\n")

	return b.String()
}

// GenerateRcloneConfig produces an rclone config file for R2 access.
func GenerateRcloneConfig(cfg R2Config) string {
	return fmt.Sprintf(`[r2]
type = s3
provider = Cloudflare
access_key_id = %s
secret_access_key = %s
endpoint = https://%s.r2.cloudflarestorage.com
`, cfg.AccessKeyID, cfg.SecretAccessKey, cfg.AccountID)
}
