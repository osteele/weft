package dataplane

import "fmt"

// Campaign keys

func CampaignManifest(instanceID int64) string {
	return fmt.Sprintf("campaigns/%d/manifest.json", instanceID)
}

func CampaignPrefix(instanceID int64) string {
	return fmt.Sprintf("campaigns/%d/", instanceID)
}

// Job artifact keys

func JobPrefix(jobID int64) string {
	return fmt.Sprintf("jobs/%d", jobID)
}

func JobRunPrefix(jobID, runID int64) string {
	if runID <= 0 {
		return JobPrefix(jobID)
	}
	return fmt.Sprintf("jobs/%d/runs/%d", jobID, runID)
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

func JobLiveTimeseries(jobID int64) string {
	return fmt.Sprintf("jobs/%d/timeseries.jsonl", jobID)
}

func JobLiveTelemetry(jobID int64) string {
	return fmt.Sprintf("jobs/%d/telemetry.jsonl", jobID)
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

func JobAttemptMaintenance(jobID, runID int64) string {
	return JobAttemptResultsPrefix(jobID, runID) + "maintenance.json"
}

// Instance artifact keys

func InstanceOpslog(instanceID int64) string {
	return fmt.Sprintf("instance/%d/opslog.jsonl", instanceID)
}

func InstanceMaintenance(instanceID int64) string {
	return fmt.Sprintf("instance/%d/maintenance.json", instanceID)
}

// Bootstrap assets

func BootstrapScript(instanceID int64) string {
	return fmt.Sprintf("bootstrap/%d.sh", instanceID)
}

// Shared cache keys

func UVManifest(lockHash, platform string) string {
	return fmt.Sprintf("uv-manifests/%s/%s.json", lockHash, platform)
}

func AgentBinary(version, goos, goarch string) string {
	return fmt.Sprintf("agents/%s/%s-%s", version, goos, goarch)
}

func SourceTarball(hash string) string {
	return fmt.Sprintf("sources/%s.tar.gz", hash)
}
