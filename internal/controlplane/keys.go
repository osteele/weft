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

// Grace status states written by the agent's grace-wait loop into the
// GraceStatus marker and read by the CLI/reconciler. GraceStateWaiting means
// the agent is idle in grace polling for control messages; GraceStateRunning
// means it is executing resubmitted jobs (the marker's deadline may be stale
// while running); GraceStateCompleted means the grace session ended.
const (
	GraceStateWaiting   = "waiting"
	GraceStateRunning   = "running"
	GraceStateCompleted = "completed"
)

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

func InstanceDriverFailure(instanceID int64) string {
	return fmt.Sprintf("instance/%d/driver-failure.json", instanceID)
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

// InstanceOnStartStage is overwritten by OnStart at each major step
// (apt-ok, uv-ok, rclone-ok, onstart-deps-ready, etc). Last-write-wins
// semantics turn the R2 object into a "last successful step" marker: when
// the chain dies between probe and bootstrap.sh, the surviving value names
// the boundary. Use it to tell apt-failed from rclone-install-failed
// without log access to the container.
func InstanceOnStartStage(instanceID int64) string {
	return fmt.Sprintf("instance/%d/onstart-stage", instanceID)
}

func InstanceTerminationIntent(instanceID int64) string {
	return fmt.Sprintf("instance/%d/termination-intent.json", instanceID)
}

// InstanceUploadFailure is written by the agent when an instance-wide
// upload drain (opslog, maintenance report, etc.) hit the stall watchdog
// or the bytes-derived ceiling. JSON shape is r2upload.FailureMarker.
func InstanceUploadFailure(instanceID int64) string {
	return fmt.Sprintf("instance/%d/upload-failure.json", instanceID)
}

// JobAttemptUploadFailure is written when a per-attempt drain (logs,
// output dirs, artifacts, or results) hit the watchdog or ceiling. The
// presence of this object means "some bytes were lost — see reason
// inside" and is surfaced by `weft job show` and `weft instance diagnose`.
func JobAttemptUploadFailure(jobID, runID int64) string {
	if runID <= 0 {
		return fmt.Sprintf("jobs/%d/upload-failure.json", jobID)
	}
	return fmt.Sprintf("%s/upload-failure.json", JobRunPrefix(jobID, runID))
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

// Federated R2 blackboard keys

func BlackboardPrefix() string {
	return "blackboard/v1/"
}

func BlackboardAutopilotState() string {
	return BlackboardPrefix() + "state/autopilot.json"
}

func BlackboardJobsPrefix() string {
	return BlackboardPrefix() + "jobs/"
}

func BlackboardJobPrefix(jobID int64) string {
	return fmt.Sprintf("%sjobs/%d/", BlackboardPrefix(), jobID)
}

func BlackboardJobSpec(jobID int64) string {
	return BlackboardJobPrefix(jobID) + "spec.json"
}

func BlackboardJobClaim(jobID int64) string {
	return BlackboardJobPrefix(jobID) + "claim.json"
}

func BlackboardJobAssignment(jobID int64) string {
	return BlackboardJobPrefix(jobID) + "assignment.json"
}

func BlackboardAgentsPrefix() string {
	return BlackboardPrefix() + "agents/"
}

func BlackboardAgentHeartbeat(agentID string) string {
	return fmt.Sprintf("%sagents/%s/heartbeat.json", BlackboardPrefix(), agentID)
}

func BlackboardEventsPrefix() string {
	return BlackboardPrefix() + "events/"
}

func BlackboardEvent(eventID string) string {
	return fmt.Sprintf("%sevents/%s.json", BlackboardPrefix(), eventID)
}
