package cloud

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AgentJob describes a job for the agent-based wrapper, serialized as JSON for stdin.
type AgentJob struct {
	ID      int64  `json:"id"`
	Command string `json:"cmd"`
	Dir     string `json:"dir,omitempty"`
}

// GenerateAgentWrapper produces a thin bash script that delegates job execution
// to the weft-agent binary. The agent handles process management, telemetry
// sampling, failure detection, and completion records. The wrapper handles only
// R2 upload and self-destruct.
func GenerateAgentWrapper(client Client, jobs []AgentJob, r2Bucket string) string {
	var b strings.Builder
	b.WriteString("#!/bin/bash\n")
	b.WriteString("set -o pipefail\n\n")

	b.WriteString(fmt.Sprintf("R2_BUCKET=%q\n", r2Bucket))
	b.WriteString("LOG_DIR=\"/tmp/weft-logs\"\n")
	b.WriteString("mkdir -p \"$LOG_DIR\"\n\n")

	for _, job := range jobs {
		b.WriteString(fmt.Sprintf("# --- Job %d ---\n", job.ID))
		b.WriteString(fmt.Sprintf("JOB_ID=%d\n", job.ID))

		// Marshal job to JSON for stdin
		jobData, _ := json.Marshal(job)
		jobJSON := string(jobData)

		workingDirFlag := ""
		if job.Dir != "" {
			workingDirFlag = fmt.Sprintf(" --working-dir=%s", job.Dir)
		} else {
			workingDirFlag = fmt.Sprintf(" --working-dir=%s", client.WorkspacePath())
		}

		b.WriteString(fmt.Sprintf("echo '%s' | weft-agent run-job --job-id=$JOB_ID --log-dir=$LOG_DIR%s\n", jobJSON, workingDirFlag))
		b.WriteString("\n")

		// Upload per-job results
		b.WriteString("# Upload per-job results\n")
		b.WriteString("rclone copy $LOG_DIR/ \"r2:$R2_BUCKET/jobs/$JOB_ID/results/\" 2>/dev/null\n")
		b.WriteString("echo \"done\" | rclone rcat \"r2:$R2_BUCKET/jobs/$JOB_ID/.complete\"\n")
		b.WriteString("rm -f $LOG_DIR/*\n\n")
	}

	// Instance completion marker (uses first job's instance_id context)
	b.WriteString("# Instance completion marker + self-destruct\n")
	b.WriteString("echo \"0\" | rclone rcat \"r2:$R2_BUCKET/campaigns/$INSTANCE_ID/.complete\"\n")
	b.WriteString(client.SelfDestructCmd())
	b.WriteString("\n")

	return b.String()
}
