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

// PidFile returns the pid file path for a job
func PidFile(jobID int64, startTime int64) string {
	return fmt.Sprintf("%s/%s.pid", LogDir, FileBasename(jobID, startTime))
}

// StatusFilePattern returns a glob pattern to find status files for a job ID
// This is useful for queued jobs where the exact timestamp is unknown
func StatusFilePattern(jobID int64) string {
	return fmt.Sprintf("%s/%d-*.status", LogDir, jobID)
}

// LogFilePattern returns a glob pattern to find log files for a job ID
// This is useful for queued jobs where the exact timestamp is unknown
func LogFilePattern(jobID int64) string {
	return fmt.Sprintf("%s/%d-*.log", LogDir, jobID)
}

// PidFilePattern returns a glob pattern to find PID files for a job ID
// This is useful for queued jobs where the exact timestamp is unknown
func PidFilePattern(jobID int64) string {
	return fmt.Sprintf("%s/%d-*.pid", LogDir, jobID)
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

// JobPidFile returns the pid file path for a job (new jobs only, no legacy support)
func JobPidFile(jobID int64, startTime int64) string {
	return PidFile(jobID, startTime)
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

// ParseCdCommand checks if a command starts with "cd <dir> &&" pattern.
// Returns (command_after_and, cd_directory) if pattern matches, or ("", "") if not.
func ParseCdCommand(cmd string) (command, dir string) {
	cmd = strings.TrimSpace(cmd)

	// Check for "cd " prefix
	if !strings.HasPrefix(cmd, "cd ") {
		return "", ""
	}

	// Find the " && " separator
	andIdx := strings.Index(cmd, " && ")
	if andIdx == -1 {
		return "", ""
	}

	// Extract the directory from "cd <dir>"
	cdPart := cmd[3:andIdx] // Skip "cd "
	dir = strings.TrimSpace(cdPart)

	// Handle quoted directories
	if (strings.HasPrefix(dir, "'") && strings.HasSuffix(dir, "'")) ||
		(strings.HasPrefix(dir, "\"") && strings.HasSuffix(dir, "\"")) {
		dir = dir[1 : len(dir)-1]
	}

	// Extract the command after " && "
	command = strings.TrimSpace(cmd[andIdx+4:])
	return command, dir
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

	// Compute display_dir and display_cmd (parsing "cd <dir> && <cmd>" pattern)
	displayCmd, displayDir := ParseCdCommand(command)
	if displayCmd != "" {
		lines = append(lines, fmt.Sprintf("display_dir=%s", displayDir))
		lines = append(lines, fmt.Sprintf("display_cmd=%s", displayCmd))
	} else {
		// No cd prefix, use working_dir and command as-is
		lines = append(lines, fmt.Sprintf("display_dir=%s", workingDir))
		lines = append(lines, fmt.Sprintf("display_cmd=%s", command))
	}

	return strings.Join(lines, "\n")
}

// WrapperCommandParams contains parameters for building a wrapper command
type WrapperCommandParams struct {
	JobID      int64
	WorkingDir string
	Command    string
	LogFile    string
	StatusFile string
	PidFile    string
	NotifyCmd  string // Optional notification command to run after job completes
}

// BuildWrapperCommand creates the bash command that wraps a job with logging,
// PID capture, and exit code handling.
//
// IMPORTANT: File paths containing ~ must NOT be quoted to allow shell expansion.
// The working directory is also unquoted to support tilde expansion.
// This function has unit tests to prevent regressions on quoting behavior.
func BuildWrapperCommand(params WrapperCommandParams) string {
	// Note: file paths use ~ which must not be quoted to allow expansion
	// The command runs in a subshell that writes its PID then execs bash -c
	// This ensures the recorded PID is the actual job process, not a wrapper
	// The command is escaped for use in single quotes passed to bash -c
	escapedCmd := escapeForBashC(params.Command)
	return fmt.Sprintf(
		`echo "=== START $(date) ===" > %s; `+
			`echo "job_id: %d" >> %s; `+
			`echo "cd: %s" >> %s; `+
			`echo "cmd: %s" >> %s; `+
			`echo "===" >> %s; `+
			`cd %s && { (echo $BASHPID > %s; exec bash -c '%s') >> %s 2>&1 & wait $!; }; `+
			`EXIT_CODE=$?; `+
			`echo "=== END exit=$EXIT_CODE $(date) ===" >> %s; `+
			`echo $EXIT_CODE > %s%s`,
		params.LogFile,
		params.JobID, params.LogFile,
		params.WorkingDir, params.LogFile,
		params.Command, params.LogFile,
		params.LogFile,
		params.WorkingDir, params.PidFile, escapedCmd, params.LogFile,
		params.LogFile,
		params.StatusFile, params.NotifyCmd)
}

// escapeForBashC escapes a command for use in bash -c '...'
func escapeForBashC(s string) string {
	// Replace single quotes with '\'' (end quote, escaped quote, start quote)
	return strings.ReplaceAll(s, "'", `'\''`)
}
