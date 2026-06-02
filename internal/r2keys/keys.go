package r2keys

import (
	"github.com/osteele/weft/internal/controlplane"
	"github.com/osteele/weft/internal/dataplane"
)

// Package r2keys is a compatibility shim while call sites migrate toward
// explicit data-plane and control-plane packages.

func CampaignManifest(instanceID int64) string { return dataplane.CampaignManifest(instanceID) }
func CampaignComplete(instanceID int64) string { return controlplane.CampaignComplete(instanceID) }
func CampaignPrefix(instanceID int64) string   { return dataplane.CampaignPrefix(instanceID) }

func JobStarted(jobID int64) string   { return controlplane.JobStarted(jobID) }
func JobComplete(jobID int64) string  { return controlplane.JobComplete(jobID) }
func JobProcessed(jobID int64) string { return controlplane.JobProcessed(jobID) }

func JobResultsPrefix(jobID int64) string { return dataplane.JobResultsPrefix(jobID) }
func JobResultLog(jobID int64) string     { return dataplane.JobResultLog(jobID) }

func JobLiveLogsPrefix(jobID int64) string        { return dataplane.JobLiveLogsPrefix(jobID) }
func JobLiveLogManifest(jobID int64) string       { return dataplane.JobLiveLogManifest(jobID) }
func JobLiveLogPart(jobID int64, part int) string { return dataplane.JobLiveLogPart(jobID, part) }
func JobOutputsPrefix(jobID int64) string         { return dataplane.JobOutputsPrefix(jobID) }
func JobOutputDir(jobID int64, dir string) string { return dataplane.JobOutputDir(jobID, dir) }
func JobProgress(jobID int64) string              { return controlplane.JobProgress(jobID) }
func JobAttemptProgress(jobID, runID int64) string {
	return controlplane.JobAttemptProgress(jobID, runID)
}
func JobLiveTimeseries(jobID int64) string   { return dataplane.JobLiveTimeseries(jobID) }
func JobLiveTelemetry(jobID int64) string    { return dataplane.JobLiveTelemetry(jobID) }
func JobPrefix(jobID int64) string           { return dataplane.JobPrefix(jobID) }
func JobRunPrefix(jobID, runID int64) string { return dataplane.JobRunPrefix(jobID, runID) }
func JobAttemptStarted(jobID, runID int64) string {
	return controlplane.JobAttemptStarted(jobID, runID)
}
func JobAttemptComplete(jobID, runID int64) string {
	return controlplane.JobAttemptComplete(jobID, runID)
}
func JobAttemptProcessed(jobID, runID int64) string {
	return controlplane.JobAttemptProcessed(jobID, runID)
}
func ExtractRunID(key string) int64 { return controlplane.ExtractRunID(key) }

func JobAttemptResultsPrefix(jobID, runID int64) string {
	return dataplane.JobAttemptResultsPrefix(jobID, runID)
}
func JobAttemptResultLog(jobID, runID int64) string {
	return dataplane.JobAttemptResultLog(jobID, runID)
}
func JobAttemptLiveLogsPrefix(jobID, runID int64) string {
	return dataplane.JobAttemptLiveLogsPrefix(jobID, runID)
}
func JobAttemptLiveLogManifest(jobID, runID int64) string {
	return dataplane.JobAttemptLiveLogManifest(jobID, runID)
}
func JobAttemptLiveLogPart(jobID, runID int64, part int) string {
	return dataplane.JobAttemptLiveLogPart(jobID, runID, part)
}
func JobAttemptOutputsPrefix(jobID, runID int64) string {
	return dataplane.JobAttemptOutputsPrefix(jobID, runID)
}
func JobAttemptOutputDir(jobID, runID int64, dir string) string {
	return dataplane.JobAttemptOutputDir(jobID, runID, dir)
}
func JobAttemptArtifactsPrefix(jobID, runID int64) string {
	return dataplane.JobAttemptArtifactsPrefix(jobID, runID)
}
func JobAttemptArtifactFilesPrefix(jobID, runID int64) string {
	return dataplane.JobAttemptArtifactFilesPrefix(jobID, runID)
}
func JobAttemptArtifactManifest(jobID, runID int64) string {
	return dataplane.JobAttemptArtifactManifest(jobID, runID)
}
func JobAttemptLiveTimeseries(jobID, runID int64) string {
	return dataplane.JobAttemptLiveTimeseries(jobID, runID)
}
func JobAttemptLiveTelemetry(jobID, runID int64) string {
	return dataplane.JobAttemptLiveTelemetry(jobID, runID)
}
func JobAttemptMaintenance(jobID, runID int64) string {
	return dataplane.JobAttemptMaintenance(jobID, runID)
}

func GracePrefix(instanceID int64) string  { return controlplane.GracePrefix(instanceID) }
func GraceStatus(instanceID int64) string  { return controlplane.GraceStatus(instanceID) }
func GraceJobs(instanceID int64) string    { return controlplane.GraceJobs(instanceID) }
func GraceRelease(instanceID int64) string { return controlplane.GraceRelease(instanceID) }
func GraceExtend(instanceID int64) string  { return controlplane.GraceExtend(instanceID) }
func GraceAck(instanceID int64) string     { return controlplane.GraceAck(instanceID) }

func InstanceAgentVersion(instanceID int64) string {
	return controlplane.InstanceAgentVersion(instanceID)
}
func InstancePhase(instanceID int64) string     { return controlplane.InstancePhase(instanceID) }
func InstanceHeartbeat(instanceID int64) string { return controlplane.InstanceHeartbeat(instanceID) }
func InstanceAgentDied(instanceID int64) string { return controlplane.InstanceAgentDied(instanceID) }
func InstanceLastSeen(instanceID int64) string  { return controlplane.InstanceLastSeen(instanceID) }
func InstanceAgentStartup(instanceID int64) string {
	return controlplane.InstanceAgentStartup(instanceID)
}
func InstanceOpslog(instanceID int64) string { return dataplane.InstanceOpslog(instanceID) }
func InstanceMaintenance(instanceID int64) string {
	return dataplane.InstanceMaintenance(instanceID)
}
func InstanceDiskFailure(instanceID int64) string {
	return controlplane.InstanceDiskFailure(instanceID)
}

func InstanceDiskCapFailure(instanceID int64) string {
	return controlplane.InstanceDiskCapFailure(instanceID)
}

func InstanceDriverFailure(instanceID int64) string {
	return controlplane.InstanceDriverFailure(instanceID)
}

func InstanceOnStartProbe(instanceID int64) string {
	return controlplane.InstanceOnStartProbe(instanceID)
}

func InstanceOnStartStage(instanceID int64) string {
	return controlplane.InstanceOnStartStage(instanceID)
}

func InstanceTerminationIntent(instanceID int64) string {
	return controlplane.InstanceTerminationIntent(instanceID)
}

func InstanceUploadFailure(instanceID int64) string {
	return controlplane.InstanceUploadFailure(instanceID)
}

func JobAttemptUploadFailure(jobID, runID int64) string {
	return controlplane.JobAttemptUploadFailure(jobID, runID)
}

func InstanceKillJob(instanceID int64) string { return controlplane.InstanceKillJob(instanceID) }
func BootstrapScript(instanceID int64) string { return dataplane.BootstrapScript(instanceID) }
func BootstrapStage(instanceID int64) string  { return controlplane.BootstrapStage(instanceID) }
func DonorReady(instanceID int64) string      { return controlplane.DonorReady(instanceID) }
func UVManifest(lockHash, platform string) string {
	return dataplane.UVManifest(lockHash, platform)
}
func AgentBinary(version, goos, goarch string) string {
	return dataplane.AgentBinary(version, goos, goarch)
}
func SourceTarball(hash string) string   { return dataplane.SourceTarball(hash) }
func NamedAsset(sha256Hex string) string { return dataplane.NamedAsset(sha256Hex) }

func CoordinatorRelayRequest(requestID string) string {
	return controlplane.CoordinatorRelayRequest(requestID)
}
func CoordinatorRelayAck(requestID string) string { return controlplane.CoordinatorRelayAck(requestID) }
func CoordinatorRelayInboxPrefix() string         { return controlplane.CoordinatorRelayInboxPrefix() }
func CoordinatorRelayAckPrefix() string           { return controlplane.CoordinatorRelayAckPrefix() }

func BlackboardPrefix() string                   { return controlplane.BlackboardPrefix() }
func BlackboardAutopilotState() string           { return controlplane.BlackboardAutopilotState() }
func BlackboardJobsPrefix() string               { return controlplane.BlackboardJobsPrefix() }
func BlackboardJobPrefix(jobID int64) string     { return controlplane.BlackboardJobPrefix(jobID) }
func BlackboardJobSpec(jobID int64) string       { return controlplane.BlackboardJobSpec(jobID) }
func BlackboardJobClaim(jobID int64) string      { return controlplane.BlackboardJobClaim(jobID) }
func BlackboardJobAssignment(jobID int64) string { return controlplane.BlackboardJobAssignment(jobID) }
func BlackboardAgentsPrefix() string             { return controlplane.BlackboardAgentsPrefix() }
func BlackboardAgentHeartbeat(agentID string) string {
	return controlplane.BlackboardAgentHeartbeat(agentID)
}
func BlackboardEventsPrefix() string        { return controlplane.BlackboardEventsPrefix() }
func BlackboardEvent(eventID string) string { return controlplane.BlackboardEvent(eventID) }
