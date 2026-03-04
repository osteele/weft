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

	for i, job := range jobs {
		b.WriteString(fmt.Sprintf("# === Job %d (ID: %d) ===\n", i+1, job.ID))
		b.WriteString(fmt.Sprintf("JOB_ID=%d\n", job.ID))
		b.WriteString("R2_PREFIX=\"jobs/$JOB_ID\"\n")

		if !continueOnFailure {
			b.WriteString("if [ $CAMPAIGN_FAILED -eq 0 ]; then\n")
		}

		b.WriteString("echo \"[campaign] Starting job $JOB_ID\"\n")
		b.WriteString("cd /workspace\n")
		b.WriteString(fmt.Sprintf("{ %s ; } > /tmp/stdout-$JOB_ID.log 2> /tmp/stderr-$JOB_ID.log\n", job.Command))
		b.WriteString("JOB_EXIT=$?\n")
		b.WriteString("echo $JOB_EXIT > /tmp/exit-$JOB_ID\n")
		b.WriteString("date -u +%s > /tmp/end-$JOB_ID\n\n")

		// Upload per-job results
		b.WriteString("# Upload job results\n")
		b.WriteString("mkdir -p /tmp/results-$JOB_ID\n")
		b.WriteString("cp /tmp/stdout-$JOB_ID.log /tmp/stderr-$JOB_ID.log /tmp/exit-$JOB_ID /tmp/end-$JOB_ID /tmp/results-$JOB_ID/\n")
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

	// Campaign completion marker
	b.WriteString("# Campaign completion marker\n")
	b.WriteString("echo \"$CAMPAIGN_FAILED\" | rclone rcat \"r2:$R2_BUCKET/campaigns/$CAMPAIGN_ID/.complete\"\n\n")

	// Self-destruct
	b.WriteString("# Self-destruct\n")
	b.WriteString("vastai destroy instance \"$CONTAINER_ID\" --api-key \"$CONTAINER_API_KEY\" 2>/dev/null || true\n")

	return b.String()
}
