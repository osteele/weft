package controlplane

import (
	"fmt"
	"strconv"
	"strings"
)

// Campaign state keys

func CampaignComplete(instanceID int64) string {
	return fmt.Sprintf("campaigns/%d/.complete", instanceID)
}

// Job state keys

func JobPrefix(jobID int64) string {
	return fmt.Sprintf("jobs/%d", jobID)
}

func JobRunPrefix(jobID, runID int64) string {
	if runID <= 0 {
		return JobPrefix(jobID)
	}
	return fmt.Sprintf("jobs/%d/runs/%d", jobID, runID)
}

func JobStarted(jobID int64) string {
	return fmt.Sprintf("jobs/%d/.started", jobID)
}

func JobComplete(jobID int64) string {
	return fmt.Sprintf("jobs/%d/.complete", jobID)
}

func JobProcessed(jobID int64) string {
	return fmt.Sprintf("jobs/%d/.processed", jobID)
}

func JobProgress(jobID int64) string {
	return fmt.Sprintf("jobs/%d/progress", jobID)
}

func JobAttemptStarted(jobID, runID int64) string {
	if runID <= 0 {
		return JobStarted(jobID)
	}
	return fmt.Sprintf("%s/.started", JobRunPrefix(jobID, runID))
}

func JobAttemptComplete(jobID, runID int64) string {
	if runID <= 0 {
		return JobComplete(jobID)
	}
	return fmt.Sprintf("%s/.complete", JobRunPrefix(jobID, runID))
}

func JobAttemptProcessed(jobID, runID int64) string {
	if runID <= 0 {
		return JobProcessed(jobID)
	}
	return fmt.Sprintf("%s/.processed", JobRunPrefix(jobID, runID))
}

func JobAttemptProgress(jobID, runID int64) string {
	if runID <= 0 {
		return JobProgress(jobID)
	}
	return fmt.Sprintf("%s/progress", JobRunPrefix(jobID, runID))
}

// ExtractRunID parses the run_id from a job R2 key such as
// "jobs/441/runs/123/.complete" -> 123, or "jobs/441/.complete" -> 0.
func ExtractRunID(key string) int64 {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		if p == "runs" && i+1 < len(parts) {
			id, err := strconv.ParseInt(parts[i+1], 10, 64)
			if err == nil {
				return id
			}
		}
	}
	return 0
}

// Grace-period control keys

func GracePrefix(instanceID int64) string {
	return fmt.Sprintf("grace/%d", instanceID)
}

func GraceStatus(instanceID int64) string {
	return fmt.Sprintf("grace/%d/status", instanceID)
}

func GraceJobs(instanceID int64) string {
	return fmt.Sprintf("grace/%d/jobs.json", instanceID)
}

func GraceRelease(instanceID int64) string {
	return fmt.Sprintf("grace/%d/release", instanceID)
}

func GraceExtend(instanceID int64) string {
	return fmt.Sprintf("grace/%d/extend", instanceID)
}

func GraceAck(instanceID int64) string {
	return fmt.Sprintf("grace/%d/ack", instanceID)
}

// Instance state keys

func InstanceAgentVersion(instanceID int64) string {
	return fmt.Sprintf("instance/%d/agent-version", instanceID)
}

func InstancePhase(instanceID int64) string {
	return fmt.Sprintf("instance/%d/phase", instanceID)
}

func InstanceHeartbeat(instanceID int64) string {
	return fmt.Sprintf("instance/%d/heartbeat", instanceID)
}

func InstanceAgentDied(instanceID int64) string {
	return fmt.Sprintf("instance/%d/agent-died.json", instanceID)
}

// InstanceLastSeen is a minimal liveness ping written by the heartbeat
// sidecar before any expensive metric collection. It exists so a hung
// nvidia-smi or other sample-collection blockage does not silence our
// only liveness signal — the sidecar can still push a fresh timestamp
// even if the full heartbeat sample never assembles.
func InstanceLastSeen(instanceID int64) string {
	return fmt.Sprintf("instance/%d/last-seen", instanceID)
}

func InstanceAgentStartup(instanceID int64) string {
	return fmt.Sprintf("instance/%d/agent-startup.json", instanceID)
}

func InstanceDiskFailure(instanceID int64) string {
	return fmt.Sprintf("instance/%d/disk-failure.json", instanceID)
}

func InstanceDiskCapFailure(instanceID int64) string {
	return fmt.Sprintf("instance/%d/disk-cap-failure.json", instanceID)
}

// InstanceOnStartProbe is written by the very first line of OnStart via a
// presigned PUT URL. Its presence proves the container ran OnStart and had
// outbound network at all — independent of whether rclone is installed,
// configured, or working. Used to disambiguate "OnStart never ran" from
// "OnStart ran but rclone failed" when an instance dies with no other
// markers in R2.
func InstanceOnStartProbe(instanceID int64) string {
	return fmt.Sprintf("instance/%d/onstart-probe", instanceID)
}

func InstanceTerminationIntent(instanceID int64) string {
	return fmt.Sprintf("instance/%d/termination-intent.json", instanceID)
}

func InstanceKillJob(instanceID int64) string {
	return fmt.Sprintf("instance/%d/kill-job", instanceID)
}

// Bootstrap/runtime coordination keys

func BootstrapStage(instanceID int64) string {
	return fmt.Sprintf("bootstrap/%d/stage", instanceID)
}

func DonorReady(instanceID int64) string {
	return fmt.Sprintf("donor/%d/.ready", instanceID)
}

// Coordinator relay keys

func CoordinatorRelayRequest(requestID string) string {
	return fmt.Sprintf("coordinator/v1/inbox/%s.json", requestID)
}

func CoordinatorRelayAck(requestID string) string {
	return fmt.Sprintf("coordinator/v1/acks/%s.json", requestID)
}

func CoordinatorRelayInboxPrefix() string {
	return "coordinator/v1/inbox/"
}

func CoordinatorRelayAckPrefix() string {
	return "coordinator/v1/acks/"
}
