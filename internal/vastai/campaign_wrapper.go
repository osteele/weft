package vastai

import (
	"fmt"
	"strings"
)

// CampaignJob describes a job to be included in a multi-job campaign wrapper.
type CampaignJob struct {
	ID      int64
	Command string
}

// GenerateCampaignWrapper produces a bash script that runs multiple jobs sequentially
// on a single Vast.ai instance. Each job's results are uploaded to R2 individually,
// and the campaign is marked complete after all jobs finish.
//
// On any job failure, remaining jobs are skipped (unless continueOnFailure is set).
func GenerateCampaignWrapper(campaignID int64, jobs []CampaignJob, r2Bucket string, continueOnFailure bool) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -o pipefail\n\n")

	b.WriteString(fmt.Sprintf("CAMPAIGN_ID=%d\n", campaignID))
	b.WriteString(fmt.Sprintf("R2_BUCKET=%q\n", r2Bucket))
	b.WriteString("CAMPAIGN_FAILED=0\n\n")

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
		b.WriteString("cd /workspace\n")

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

		// Upload per-job results
		b.WriteString("# Upload job results\n")
		b.WriteString("mkdir -p /tmp/results-$JOB_ID\n")
		b.WriteString("cp /tmp/stdout-$JOB_ID.log /tmp/stderr-$JOB_ID.log /tmp/exit-$JOB_ID /tmp/end-$JOB_ID /tmp/results-$JOB_ID/\n")
		b.WriteString("cp /tmp/phase_run_start_$JOB_ID /tmp/phase_run_end_$JOB_ID /tmp/results-$JOB_ID/ 2>/dev/null\n")
		b.WriteString("cp /tmp/cache_hf_$JOB_ID /tmp/cache_uv_$JOB_ID /tmp/gpu_monitor_$JOB_ID.csv /tmp/results-$JOB_ID/ 2>/dev/null\n")
		b.WriteString("du -sb /tmp/results-$JOB_ID/ 2>/dev/null | cut -f1 > /tmp/results-$JOB_ID/upload_results_bytes\n")
		b.WriteString("du -sb /workspace/ 2>/dev/null | cut -f1 > /tmp/results-$JOB_ID/upload_workspace_bytes\n")
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
	b.WriteString("vastai destroy instance \"$CONTAINER_ID\" --api-key \"$CONTAINER_API_KEY\" 2>/dev/null || true\n")

	return b.String()
}
