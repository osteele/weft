package session

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// LogDir is the directory for job logs on remote hosts
const LogDir = "~/.cache/remote-jobs/logs"

// TmuxSessionName returns the tmux session name for a job ID
func TmuxSessionName(jobID int64) string {
	return fmt.Sprintf("rj-%d", jobID)
}

// FileBasename returns the base filename for job files (without extension)
// Format: {jobID}-{timestamp}
func FileBasename(jobID int64, startTime int64) string {
	t := time.Unix(startTime, 0)
	return fmt.Sprintf("%d-%s", jobID, t.Format("20060102-150405"))
}

// DefaultWorkingDir returns the current working directory converted to a remote-friendly path
// /Users/osteele/code/LM2 -> ~/code/LM2
func DefaultWorkingDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}

	if strings.HasPrefix(cwd, home) {
		return "~" + cwd[len(home):], nil
	}

	return cwd, nil
}

// LogFile returns the log file path for a job
func LogFile(jobID int64, startTime int64) string {
	return fmt.Sprintf("%s/%s.log", LogDir, FileBasename(jobID, startTime))
}

// StatusFile returns the status file path for a job
func StatusFile(jobID int64, startTime int64) string {
	return fmt.Sprintf("%s/%s.status", LogDir, FileBasename(jobID, startTime))
}

// MetadataFile returns the metadata file path for a job
func MetadataFile(jobID int64, startTime int64) string {
	return fmt.Sprintf("%s/%s.meta", LogDir, FileBasename(jobID, startTime))
}

// LegacyLogFile returns the old-style log file path for backward compatibility
func LegacyLogFile(sessionName string) string {
	return fmt.Sprintf("/tmp/tmux-%s.log", sessionName)
}

// LegacyStatusFile returns the old-style status file path for backward compatibility
func LegacyStatusFile(sessionName string) string {
	return fmt.Sprintf("/tmp/tmux-%s.status", sessionName)
}

// LegacyMetadataFile returns the old-style metadata file path for backward compatibility
func LegacyMetadataFile(sessionName string) string {
	return fmt.Sprintf("/tmp/tmux-%s.meta", sessionName)
}

// JobLogFile returns the appropriate log file path for a job (handles legacy and new)
func JobLogFile(jobID int64, startTime int64, sessionName string) string {
	if sessionName != "" {
		return LegacyLogFile(sessionName)
	}
	return LogFile(jobID, startTime)
}

// JobStatusFile returns the appropriate status file path for a job (handles legacy and new)
func JobStatusFile(jobID int64, startTime int64, sessionName string) string {
	if sessionName != "" {
		return LegacyStatusFile(sessionName)
	}
	return StatusFile(jobID, startTime)
}

// JobMetadataFile returns the appropriate metadata file path for a job (handles legacy and new)
func JobMetadataFile(jobID int64, startTime int64, sessionName string) string {
	if sessionName != "" {
		return LegacyMetadataFile(sessionName)
	}
	return MetadataFile(jobID, startTime)
}

// JobTmuxSession returns the tmux session name for a job (handles legacy and new)
func JobTmuxSession(jobID int64, sessionName string) string {
	if sessionName != "" {
		return sessionName
	}
	return TmuxSessionName(jobID)
}

// ParseMetadata parses a metadata file content into key-value pairs
func ParseMetadata(content string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		if idx := strings.Index(line, "="); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			value := strings.TrimSpace(line[idx+1:])
			result[key] = value
		}
	}
	return result
}

// FormatMetadata formats metadata as key=value pairs
func FormatMetadata(jobID int64, workingDir, command, host, description string, startTime int64) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("job_id=%d", jobID))
	lines = append(lines, fmt.Sprintf("working_dir=%s", workingDir))
	lines = append(lines, fmt.Sprintf("command=%s", command))
	lines = append(lines, fmt.Sprintf("start_time=%d", startTime))
	lines = append(lines, fmt.Sprintf("host=%s", host))
	if description != "" {
		lines = append(lines, fmt.Sprintf("description=%s", description))
	}
	return strings.Join(lines, "\n")
}
