package cloud

import (
	"fmt"
	"strings"
)

// uvTimingShim is injected into wrapper scripts to intercept `uv sync` calls
// and log their duration to /tmp/uv_sync_seconds (one line per invocation).
const uvTimingShim = `# uv sync timing shim
mkdir -p /tmp/bin
cat > /tmp/bin/uv << 'SHIMEOF'
#!/bin/bash
UV_REAL=$(which -a uv | grep -v /tmp/bin | head -1)
if [ -z "$UV_REAL" ]; then
  echo "uv not found" >&2; exit 127
fi
if [ "$1" = "sync" ]; then
  _start=$(date -u +%s)
  "$UV_REAL" "$@"
  _rc=$?
  _end=$(date -u +%s)
  echo "$((_end - _start))" >> /tmp/uv_sync_seconds
  exit $_rc
else
  exec "$UV_REAL" "$@"
fi
SHIMEOF
chmod +x /tmp/bin/uv
export PATH="/tmp/bin:$PATH"

`

// GenerateWrapper produces a bash script that runs a job command, uploads
// results to R2, and self-destructs the cloud instance.
//
// The script is deployed to {workspacePath}/.weft-runner.sh and invoked via nohup.
func GenerateWrapper(client Client, jobID int64, command string, r2Bucket string) string {
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

	// uv sync timing shim
	b.WriteString(uvTimingShim)

	// Setup phase (pre-job)
	b.WriteString("# --- Setup phase ---\n")
	b.WriteString("date -u +%s > /tmp/phase_setup_start\n")
	b.WriteString(fmt.Sprintf("cd %s\n", client.WorkspacePath()))
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

	// Post-job cache sizes
	b.WriteString("# Post-job cache sizes\n")
	b.WriteString("du -sb ~/.cache/uv 2>/dev/null | cut -f1 > /tmp/cache_uv_post || echo 0 > /tmp/cache_uv_post\n")
	b.WriteString("du -sb ~/.cache/huggingface 2>/dev/null | cut -f1 > /tmp/cache_hf_post || echo 0 > /tmp/cache_hf_post\n\n")

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
	b.WriteString("cp /tmp/uv_sync_seconds /tmp/cache_uv_post /tmp/cache_hf_post /tmp/results/ 2>/dev/null\n")
	b.WriteString("[ -d /tmp/debug ] && cp -r /tmp/debug /tmp/results/\n\n")

	// Record upload sizes
	b.WriteString("# Record upload sizes\n")
	b.WriteString("du -sb /tmp/results/ 2>/dev/null | cut -f1 > /tmp/results/upload_results_bytes\n")
	b.WriteString(fmt.Sprintf("du -sb %s 2>/dev/null | cut -f1 > /tmp/results/upload_workspace_bytes\n\n", client.WorkspacePath()))

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
	b.WriteString(fmt.Sprintf(`rclone copy %s "r2:$R2_BUCKET/$R2_PREFIX/workspace/" 2>/dev/null`, client.WorkspacePath()))
	b.WriteString("\n")
	b.WriteString("date -u +%s > /tmp/phase_upload_end\n\n")

	// Write completion marker
	b.WriteString("# Write completion marker\n")
	b.WriteString(`echo "done" | rclone rcat "r2:$R2_BUCKET/$R2_PREFIX/.complete"`)
	b.WriteString("\n\n")

	// Self-destruct
	b.WriteString("# Self-destruct\n")
	b.WriteString(client.SelfDestructCmd())
	b.WriteString("\n")

	return b.String()
}

// GenerateCampaignWrapper produces a bash script that runs multiple jobs sequentially
// on a single cloud instance. Each job's results are uploaded to R2 individually,
// and the campaign is marked complete after all jobs finish.
func GenerateCampaignWrapper(client Client, campaignID int64, jobs []CampaignJob, r2Bucket string, continueOnFailure bool) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -o pipefail\n\n")

	b.WriteString(fmt.Sprintf("CAMPAIGN_ID=%d\n", campaignID))
	b.WriteString(fmt.Sprintf("R2_BUCKET=%q\n", r2Bucket))
	b.WriteString("CAMPAIGN_FAILED=0\n\n")

	// uv sync timing shim
	b.WriteString(uvTimingShim)

	// Phase timing: campaign start
	b.WriteString("# Phase timing\n")
	b.WriteString("date -u +%s > /tmp/phase_start\n\n")

	for i, job := range jobs {
		b.WriteString(fmt.Sprintf("# === Job %d (ID: %d) ===\n", i+1, job.ID))
		b.WriteString(fmt.Sprintf("JOB_ID=%d\n", job.ID))
		b.WriteString("R2_PREFIX=\"jobs/$JOB_ID\"\n")

		if !continueOnFailure {
			b.WriteString("if [ $CAMPAIGN_FAILED -eq 0 ]; then\n")
		}

		b.WriteString("echo \"[campaign] Starting job $JOB_ID\"\n")
		b.WriteString(fmt.Sprintf("cd %s\n", client.WorkspacePath()))

		// Cache-state probes before job
		b.WriteString("du -sb ~/.cache/huggingface 2>/dev/null | cut -f1 > /tmp/cache_hf_$JOB_ID || echo 0 > /tmp/cache_hf_$JOB_ID\n")
		b.WriteString("du -sb ~/.cache/uv 2>/dev/null | cut -f1 > /tmp/cache_uv_$JOB_ID || echo 0 > /tmp/cache_uv_$JOB_ID\n")

		b.WriteString("date -u +%s > /tmp/phase_run_start_$JOB_ID\n")

		// Start GPU monitor for this job
		b.WriteString("(while true; do\n")
		b.WriteString("    nvidia-smi --query-gpu=utilization.gpu,memory.used --format=csv,noheader,nounits >> /tmp/gpu_monitor_$JOB_ID.csv 2>/dev/null\n")
		b.WriteString("    sleep 5\n")
		b.WriteString("done) &\n")
		b.WriteString("GPU_MONITOR_PID=$!\n")

		b.WriteString(fmt.Sprintf("{ %s ; } > /tmp/stdout-$JOB_ID.log 2> /tmp/stderr-$JOB_ID.log\n", job.Command))
		b.WriteString("JOB_EXIT=$?\n")
		b.WriteString("kill $GPU_MONITOR_PID 2>/dev/null; wait $GPU_MONITOR_PID 2>/dev/null\n")
		b.WriteString("date -u +%s > /tmp/phase_run_end_$JOB_ID\n")
		b.WriteString("echo $JOB_EXIT > /tmp/exit-$JOB_ID\n")
		b.WriteString("date -u +%s > /tmp/end-$JOB_ID\n\n")

		// Post-job cache sizes and uv sync timing
		b.WriteString("du -sb ~/.cache/uv 2>/dev/null | cut -f1 > /tmp/cache_uv_post_$JOB_ID || echo 0 > /tmp/cache_uv_post_$JOB_ID\n")
		b.WriteString("du -sb ~/.cache/huggingface 2>/dev/null | cut -f1 > /tmp/cache_hf_post_$JOB_ID || echo 0 > /tmp/cache_hf_post_$JOB_ID\n")
		b.WriteString("cp /tmp/uv_sync_seconds /tmp/uv_sync_seconds_$JOB_ID 2>/dev/null\n")
		b.WriteString("> /tmp/uv_sync_seconds 2>/dev/null\n\n")

		// Upload per-job results
		b.WriteString("# Upload job results\n")
		b.WriteString("mkdir -p /tmp/results-$JOB_ID\n")
		b.WriteString("cp /tmp/stdout-$JOB_ID.log /tmp/stderr-$JOB_ID.log /tmp/exit-$JOB_ID /tmp/end-$JOB_ID /tmp/results-$JOB_ID/\n")
		b.WriteString("cp /tmp/phase_run_start_$JOB_ID /tmp/phase_run_end_$JOB_ID /tmp/results-$JOB_ID/ 2>/dev/null\n")
		b.WriteString("cp /tmp/cache_hf_$JOB_ID /tmp/cache_uv_$JOB_ID /tmp/gpu_monitor_$JOB_ID.csv /tmp/results-$JOB_ID/ 2>/dev/null\n")
		b.WriteString("cp /tmp/cache_uv_post_$JOB_ID /tmp/cache_hf_post_$JOB_ID /tmp/uv_sync_seconds_$JOB_ID /tmp/results-$JOB_ID/ 2>/dev/null\n")
		b.WriteString("du -sb /tmp/results-$JOB_ID/ 2>/dev/null | cut -f1 > /tmp/results-$JOB_ID/upload_results_bytes\n")
		b.WriteString(fmt.Sprintf("du -sb %s 2>/dev/null | cut -f1 > /tmp/results-$JOB_ID/upload_workspace_bytes\n", client.WorkspacePath()))
		b.WriteString("if [ $JOB_EXIT -ne 0 ]; then\n")
		b.WriteString("    mkdir -p /tmp/results-$JOB_ID/debug\n")
		b.WriteString("    nvidia-smi > /tmp/results-$JOB_ID/debug/nvidia-smi.log 2>/dev/null\n")
		b.WriteString("    CAMPAIGN_FAILED=1\n")
		b.WriteString("fi\n")
		b.WriteString("for attempt in 1 2 3; do\n")
		b.WriteString("    rclone copy /tmp/results-$JOB_ID/ \"r2:$R2_BUCKET/$R2_PREFIX/results/\" && break\n")
		b.WriteString("    sleep $((attempt * 5))\n")
		b.WriteString("done\n")
		b.WriteString("echo \"done\" | rclone rcat \"r2:$R2_BUCKET/$R2_PREFIX/.complete\"\n")

		if !continueOnFailure {
			b.WriteString("fi\n")
		}
		b.WriteString("\n")
	}

	// Upload campaign-level phase timing
	b.WriteString("# Upload campaign phase timing\n")
	b.WriteString("date -u +%s > /tmp/phase_end\n")
	b.WriteString("mkdir -p /tmp/campaign-results\n")
	b.WriteString("cp /tmp/phase_start /tmp/phase_end /tmp/campaign-results/\n")
	b.WriteString("rclone copy /tmp/campaign-results/ \"r2:$R2_BUCKET/campaigns/$CAMPAIGN_ID/results/\" 2>/dev/null\n\n")

	// Campaign completion marker
	b.WriteString("# Campaign completion marker\n")
	b.WriteString("echo \"$CAMPAIGN_FAILED\" | rclone rcat \"r2:$R2_BUCKET/campaigns/$CAMPAIGN_ID/.complete\"\n\n")

	// Self-destruct
	b.WriteString("# Self-destruct\n")
	b.WriteString(client.SelfDestructCmd())
	b.WriteString("\n")

	return b.String()
}
