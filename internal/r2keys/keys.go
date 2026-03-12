package r2keys

import (
	"fmt"
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

func JobLiveTimeseries(jobID int64) string {
	return fmt.Sprintf("jobs/%d/timeseries.jsonl", jobID)
}

func JobPrefix(jobID int64) string {
	return fmt.Sprintf("jobs/%d", jobID)
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

func InstanceOpslog(instanceID int64) string {
	return fmt.Sprintf("instance/%d/opslog.jsonl", instanceID)
}

func InstanceDiskFailure(instanceID int64) string {
	return fmt.Sprintf("instance/%d/disk-failure.json", instanceID)
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
