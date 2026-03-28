package r2keys

import (
	"fmt"
	"strconv"
	"strings"
)

// Campaign keys

func CampaignManifest(instanceID int64) string {
	return fmt.Sprintf("campaigns/%d/manifest.json", instanceID)
}

func CampaignComplete(instanceID int64) string {
	return fmt.Sprintf("campaigns/%d/.complete", instanceID)
}

func CampaignPrefix(instanceID int64) string {
	return fmt.Sprintf("campaigns/%d/", instanceID)
}

// Job keys

func JobStarted(jobID int64) string {
	return fmt.Sprintf("jobs/%d/.started", jobID)
}

func JobComplete(jobID int64) string {
	return fmt.Sprintf("jobs/%d/.complete", jobID)
}

func JobProcessed(jobID int64) string {
	return fmt.Sprintf("jobs/%d/.processed", jobID)
}

func JobResultsPrefix(jobID int64) string {
	return fmt.Sprintf("jobs/%d/results/", jobID)
}

func JobResultLog(jobID int64) string {
	return fmt.Sprintf("jobs/%d/results/%d.log", jobID, jobID)
}

func JobLiveLogsPrefix(jobID int64) string {
	return fmt.Sprintf("jobs/%d/live-log/", jobID)
}

func JobLiveLogManifest(jobID int64) string {
	return fmt.Sprintf("jobs/%d/live-log/manifest.json", jobID)
}

func JobLiveLogPart(jobID int64, part int) string {
	return fmt.Sprintf("jobs/%d/live-log/part-%06d.log", jobID, part)
}

func JobOutputsPrefix(jobID int64) string {
	return fmt.Sprintf("jobs/%d/outputs/", jobID)
}

func JobOutputDir(jobID int64, dir string) string {
	return fmt.Sprintf("jobs/%d/outputs/%s/", jobID, dir)
}

func JobProgress(jobID int64) string {
	return fmt.Sprintf("jobs/%d/progress", jobID)
}

func JobAttemptProgress(jobID, runID int64) string {
	if runID <= 0 {
		return JobProgress(jobID)
	}
	return fmt.Sprintf("%s/progress", JobRunPrefix(jobID, runID))
}

func JobLiveTimeseries(jobID int64) string {
	return fmt.Sprintf("jobs/%d/timeseries.jsonl", jobID)
}

func JobLiveTelemetry(jobID int64) string {
	return fmt.Sprintf("jobs/%d/telemetry.jsonl", jobID)
}

func JobPrefix(jobID int64) string {
	return fmt.Sprintf("jobs/%d", jobID)
}

func JobRunPrefix(jobID, runID int64) string {
	if runID <= 0 {
		return JobPrefix(jobID)
	}
	return fmt.Sprintf("jobs/%d/runs/%d", jobID, runID)
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

// ExtractRunID parses the run_id from a job R2 key such as
// "jobs/441/runs/123/.complete" → 123, or "jobs/441/.complete" → 0.
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

func JobAttemptResultsPrefix(jobID, runID int64) string {
	if runID <= 0 {
		return JobResultsPrefix(jobID)
	}
	return fmt.Sprintf("%s/results/", JobRunPrefix(jobID, runID))
}

func JobAttemptResultLog(jobID, runID int64) string {
	if runID <= 0 {
		return JobResultLog(jobID)
	}
	return fmt.Sprintf("%s/results/%d.log", JobRunPrefix(jobID, runID), jobID)
}

func JobAttemptLiveLogsPrefix(jobID, runID int64) string {
	if runID <= 0 {
		return JobLiveLogsPrefix(jobID)
	}
	return fmt.Sprintf("%s/live-log/", JobRunPrefix(jobID, runID))
}

func JobAttemptLiveLogManifest(jobID, runID int64) string {
	if runID <= 0 {
		return JobLiveLogManifest(jobID)
	}
	return fmt.Sprintf("%s/live-log/manifest.json", JobRunPrefix(jobID, runID))
}

func JobAttemptLiveLogPart(jobID, runID int64, part int) string {
	if runID <= 0 {
		return JobLiveLogPart(jobID, part)
	}
	return fmt.Sprintf("%s/live-log/part-%06d.log", JobRunPrefix(jobID, runID), part)
}

func JobAttemptOutputsPrefix(jobID, runID int64) string {
	if runID <= 0 {
		return JobOutputsPrefix(jobID)
	}
	return fmt.Sprintf("%s/outputs/", JobRunPrefix(jobID, runID))
}

func JobAttemptOutputDir(jobID, runID int64, dir string) string {
	if runID <= 0 {
		return JobOutputDir(jobID, dir)
	}
	return fmt.Sprintf("%s/outputs/%s/", JobRunPrefix(jobID, runID), dir)
}

func JobAttemptArtifactsPrefix(jobID, runID int64) string {
	if runID <= 0 {
		return fmt.Sprintf("jobs/%d/artifacts/", jobID)
	}
	return fmt.Sprintf("%s/artifacts/", JobRunPrefix(jobID, runID))
}

func JobAttemptArtifactFilesPrefix(jobID, runID int64) string {
	return JobAttemptArtifactsPrefix(jobID, runID) + "files/"
}

func JobAttemptArtifactManifest(jobID, runID int64) string {
	return JobAttemptArtifactsPrefix(jobID, runID) + "manifest.json"
}

func JobAttemptLiveTimeseries(jobID, runID int64) string {
	if runID <= 0 {
		return JobLiveTimeseries(jobID)
	}
	return fmt.Sprintf("%s/timeseries.jsonl", JobRunPrefix(jobID, runID))
}

func JobAttemptLiveTelemetry(jobID, runID int64) string {
	if runID <= 0 {
		return JobLiveTelemetry(jobID)
	}
	return fmt.Sprintf("%s/telemetry.jsonl", JobRunPrefix(jobID, runID))
}

// Grace period keys

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

// Instance keys

func InstanceAgentVersion(instanceID int64) string {
	return fmt.Sprintf("instance/%d/agent-version", instanceID)
}

func InstancePhase(instanceID int64) string {
	return fmt.Sprintf("instance/%d/phase", instanceID)
}

func InstanceHeartbeat(instanceID int64) string {
	return fmt.Sprintf("instance/%d/heartbeat", instanceID)
}

func InstanceAgentStartup(instanceID int64) string {
	return fmt.Sprintf("instance/%d/agent-startup.json", instanceID)
}

func InstanceOpslog(instanceID int64) string {
	return fmt.Sprintf("instance/%d/opslog.jsonl", instanceID)
}

func InstanceDiskFailure(instanceID int64) string {
	return fmt.Sprintf("instance/%d/disk-failure.json", instanceID)
}

func InstanceTerminationIntent(instanceID int64) string {
	return fmt.Sprintf("instance/%d/termination-intent.json", instanceID)
}

// Bootstrap keys

func BootstrapScript(instanceID int64) string {
	return fmt.Sprintf("bootstrap/%d.sh", instanceID)
}

func BootstrapStage(instanceID int64) string {
	return fmt.Sprintf("bootstrap/%d/stage", instanceID)
}

// Donor keys

func DonorReady(instanceID int64) string {
	return fmt.Sprintf("donor/%d/.ready", instanceID)
}

// UV manifest keys

func UVManifest(lockHash, platform string) string {
	return fmt.Sprintf("uv-manifests/%s/%s.json", lockHash, platform)
}

// Agent binary keys

func AgentBinary(version, goos, goarch string) string {
	return fmt.Sprintf("agents/%s/%s-%s", version, goos, goarch)
}

// Source tarball keys

func SourceTarball(hash string) string {
	return fmt.Sprintf("sources/%s.tar.gz", hash)
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
