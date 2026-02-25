package session

import (
	"fmt"
	"strings"
	"time"
)

// LogDir is the directory for job logs on remote hosts
const LogDir = "~/.cache/weft/logs"

// TmuxSessionName returns the tmux session name for a job ID
func TmuxSessionName(jobID int64) string {
	return fmt.Sprintf("rj-%d", jobID)
}

// SimpleLogFile returns the primary log file path for a job (no timestamp)
func SimpleLogFile(jobID int64) string {
	return fmt.Sprintf("%s/%d.log", LogDir, jobID)
}

// SimpleStatusFile returns the primary status file path for a job (no timestamp)
func SimpleStatusFile(jobID int64) string {
	return fmt.Sprintf("%s/%d.status", LogDir, jobID)
}

// SimpleMetadataFile returns the primary metadata file path for a job (no timestamp)
func SimpleMetadataFile(jobID int64) string {
	return fmt.Sprintf("%s/%d.meta", LogDir, jobID)
}

// SimplePidFile returns the primary PID file path for a job (no timestamp)
func SimplePidFile(jobID int64) string {
	return fmt.Sprintf("%s/%d.pid", LogDir, jobID)
}

// SimplePgidFile returns the primary process group ID file path for a job (no timestamp)
func SimplePgidFile(jobID int64) string {
	return fmt.Sprintf("%s/%d.pgid", LogDir, jobID)
}

// SimpleSamplesFile returns the primary samples file path for a job (no timestamp)
func SimpleSamplesFile(jobID int64) string {
	return fmt.Sprintf("%s/%d.samples", LogDir, jobID)
}

// SimplePausedFile returns the paused marker file path for a job.
// This file is created when a job is paused and removed when resumed.
// The queue runner checks for this file before treating stopped processes as failed.
func SimplePausedFile(jobID int64) string {
	return fmt.Sprintf("%s/%d.paused", LogDir, jobID)
}

// SimpleRusageFile returns the resource usage file path for a job (no timestamp)
func SimpleRusageFile(jobID int64) string {
	return fmt.Sprintf("%s/%d.rusage", LogDir, jobID)
}

// RusageFilePattern returns a glob pattern to find rusage files for a job ID
func RusageFilePattern(jobID int64) string {
	return fmt.Sprintf("%s/%d*.rusage", LogDir, jobID)
}

// ArchiveCommand returns a shell command that archives existing job files by renaming
// them with their creation date. This should be run before starting a new job run.
// Example: 123.log -> 123-20260104-095748.log
func ArchiveCommand(jobID int64) string {
	// For each extension, check if file exists and rename it with its mtime
	// Uses stat to get mtime: stat -c %Y on Linux, stat -f %m on macOS
	return fmt.Sprintf(`
		for ext in log status meta pid samples rusage; do
			f="%s/%d.$ext"
			if [ -f "$f" ]; then
				mtime=$(stat -c %%Y "$f" 2>/dev/null || stat -f %%m "$f" 2>/dev/null)
				if [ -n "$mtime" ]; then
					ts=$(date -r "$mtime" "+%%Y%%m%%d-%%H%%M%%S" 2>/dev/null || date -d "@$mtime" "+%%Y%%m%%d-%%H%%M%%S" 2>/dev/null)
					if [ -n "$ts" ]; then
						mv "$f" "%s/%d-$ts.$ext"
					fi
				fi
			fi
		done
	`, LogDir, jobID, LogDir, jobID)
}

// FileBasename returns the base filename for job files (without extension)
// Format: {jobID}-{timestamp}
// Deprecated: Use simple file paths instead. This is kept for backward compatibility.
func FileBasename(jobID int64, startTime int64) string {
	t := time.Unix(startTime, 0)
	return fmt.Sprintf("%d-%s", jobID, t.Format("20060102-150405"))
}

// DefaultWorkingDir returns an empty string to indicate no working directory should be set.
// Jobs will run from the user's home directory on the remote host.
// Previously this returned the local cwd converted to a remote path, but that path
// often doesn't exist on the remote host, causing jobs to fail.
func DefaultWorkingDir() (string, error) {
	return "", nil
}

// LogFile returns the log file path for a job
// Deprecated: Use SimpleLogFile instead. This is kept for backward compatibility.
func LogFile(jobID int64, startTime int64) string {
	return fmt.Sprintf("%s/%s.log", LogDir, FileBasename(jobID, startTime))
}

// StatusFile returns the status file path for a job
// Deprecated: Use SimpleStatusFile instead. This is kept for backward compatibility.
func StatusFile(jobID int64, startTime int64) string {
	return fmt.Sprintf("%s/%s.status", LogDir, FileBasename(jobID, startTime))
}

// MetadataFile returns the metadata file path for a job
// Deprecated: Use SimpleMetadataFile instead. This is kept for backward compatibility.
func MetadataFile(jobID int64, startTime int64) string {
	return fmt.Sprintf("%s/%s.meta", LogDir, FileBasename(jobID, startTime))
}

// PidFile returns the pid file path for a job
// Deprecated: Use SimplePidFile instead. This is kept for backward compatibility.
func PidFile(jobID int64, startTime int64) string {
	return fmt.Sprintf("%s/%s.pid", LogDir, FileBasename(jobID, startTime))
}

// StatusFilePattern returns a glob pattern to find status files for a job ID
// This matches both simple (123.status) and archived (123-*.status) files
func StatusFilePattern(jobID int64) string {
	return fmt.Sprintf("%s/%d*.status", LogDir, jobID)
}

// LogFilePattern returns a glob pattern to find log files for a job ID
// This matches both simple (123.log) and archived (123-*.log) files
func LogFilePattern(jobID int64) string {
	return fmt.Sprintf("%s/%d*.log", LogDir, jobID)
}

// SamplesFilePattern returns a glob pattern to find samples files for a job ID
// This matches both simple (123.samples) and archived (123-*.samples) files
func SamplesFilePattern(jobID int64) string {
	return fmt.Sprintf("%s/%d*.samples", LogDir, jobID)
}

// JobLogPath returns the canonical log path for a job.
// Always returns the simple path (no timestamp) since that's now the primary file.
func JobLogPath(jobID int64, startTime int64, sessionName string) (string, bool) {
	// Legacy jobs with session names use old /tmp/ paths
	if sessionName != "" && startTime == 0 {
		return LegacyLogFile(sessionName), false
	}
	return SimpleLogFile(jobID), false
}

// PidFilePattern returns a glob pattern to find PID files for a job ID
// This matches both simple (123.pid) and archived (123-*.pid) files
func PidFilePattern(jobID int64) string {
	return fmt.Sprintf("%s/%d*.pid", LogDir, jobID)
}

// MetadataFilePattern returns a glob pattern to find metadata files for a job ID
// This matches both simple (123.meta) and archived (123-*.meta) files
func MetadataFilePattern(jobID int64) string {
	return fmt.Sprintf("%s/%d*.meta", LogDir, jobID)
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

// JobLogFile returns the appropriate log file path for a job.
// Uses simple paths (no timestamp) for new jobs, legacy /tmp/ paths for old jobs.
func JobLogFile(jobID int64, startTime int64, sessionName string) string {
	// Legacy jobs without startTime - use old /tmp/ path if sessionName is set
	if startTime == 0 && sessionName != "" {
		return LegacyLogFile(sessionName)
	}
	return SimpleLogFile(jobID)
}

// JobStatusFile returns the appropriate status file path for a job.
// Uses simple paths (no timestamp) for new jobs, legacy /tmp/ paths for old jobs.
func JobStatusFile(jobID int64, startTime int64, sessionName string) string {
	// Legacy jobs without startTime - use old /tmp/ path if sessionName is set
	if startTime == 0 && sessionName != "" {
		return LegacyStatusFile(sessionName)
	}
	return SimpleStatusFile(jobID)
}

// JobMetadataFile returns the appropriate metadata file path for a job.
// Uses simple paths (no timestamp) for new jobs, legacy /tmp/ paths for old jobs.
func JobMetadataFile(jobID int64, startTime int64, sessionName string) string {
	// Legacy jobs without startTime - use old /tmp/ path if sessionName is set
	if startTime == 0 && sessionName != "" {
		return LegacyMetadataFile(sessionName)
	}
	return SimpleMetadataFile(jobID)
}

// JobPidFile returns the pid file path for a job.
// Uses simple paths (no timestamp).
func JobPidFile(jobID int64, startTime int64) string {
	return SimplePidFile(jobID)
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
	NotifyCmd  string   // Optional notification command to run after job completes
	Timeout    string   // Optional timeout duration (e.g., "2h", "30m")
	EnvVars    []string // Optional environment variables (VAR=value format)
}

// BuildWrapperCommand creates the bash command that wraps a job with logging,
// PID capture, exit code handling, and optional timeout.
//
// IMPORTANT: File paths containing ~ must NOT be quoted to allow shell expansion.
// The working directory supports both tilde expansion and spaces by replacing ~ with $HOME
// before quoting. This function has unit tests to prevent regressions on quoting behavior.
func BuildWrapperCommand(params WrapperCommandParams) string {
	// Note: file paths use ~ which must not be quoted to allow expansion
	// The command runs in a subshell that writes its PID then execs bash -c
	// This ensures the recorded PID is the actual job process, not a wrapper
	// The command is escaped for use in single quotes passed to bash -c

	// Build environment variable prefix if any env vars are specified
	envPrefix := ""
	for _, ev := range params.EnvVars {
		// Each env var is in VAR=value format, export it before the command
		envPrefix += fmt.Sprintf("export %s; ", escapeForBashC(ev))
	}

	escapedCmd := envPrefix + escapeForBashC(params.Command)

	// Prepare working directory: replace ~ with $HOME and quote for spaces
	// This allows both tilde expansion and support for spaces in paths
	workingDirQuoted := prepareWorkingDir(params.WorkingDir)

	// Build the cd prefix - either "cd <dir> &&" or empty if no dir specified
	cdPrefix := ""
	if workingDirQuoted != "" {
		cdPrefix = fmt.Sprintf("cd %s && ", workingDirQuoted)
	}

	// Build timeout monitor if timeout is specified
	timeoutMonitor := ""
	if params.Timeout != "" {
		// Timeout monitor runs in background and kills job if timeout exceeded
		// Uses GNU date for seconds since epoch (portable across Linux)
		timeoutMonitor = fmt.Sprintf(
			`{ START_TIME=$(date +%%s); TIMEOUT_SECONDS=$(echo '%s' | `+
				`sed 's/h/*3600+/g;s/m/*60+/g;s/s/*1+/g;s/+$//' | bc); `+
				`while kill -0 $(cat %s 2>/dev/null) 2>/dev/null; do `+
				`ELAPSED=$(($(date +%%s) - START_TIME)); `+
				`if [ $ELAPSED -ge $TIMEOUT_SECONDS ]; then `+
				`echo "=== TIMEOUT after %s ===" >> %s; `+
				`kill $(cat %s 2>/dev/null) 2>/dev/null; break; fi; `+
				`sleep 10; done; } & `,
			params.Timeout, params.PidFile, params.Timeout, params.LogFile, params.PidFile)
	}

	// Log working dir - use "(home)" if none specified
	logWorkingDir := params.WorkingDir
	if logWorkingDir == "" {
		logWorkingDir = "(home)"
	}

	return fmt.Sprintf(
		`echo "=== START $(date) ===" > %s; `+
			`echo "job_id: %d" >> %s; `+
			`echo "cd: %s" >> %s; `+
			`echo "cmd: %s" >> %s; `+
			`%s`+ // timeout line (empty if no timeout)
			`echo "===" >> %s; `+
			`%s`+ // timeout monitor (empty if no timeout)
			`%s{ (echo $BASHPID > %s; exec bash -c '%s') >> %s 2>&1 & wait $!; }; `+
			`EXIT_CODE=$?; `+
			`echo "=== END exit=$EXIT_CODE $(date) ===" >> %s; `+
			`echo $EXIT_CODE > %s%s`,
		params.LogFile,
		params.JobID, params.LogFile,
		logWorkingDir, params.LogFile,
		params.Command, params.LogFile,
		func() string {
			if params.Timeout != "" {
				return fmt.Sprintf(`echo "timeout: %s" >> %s; `, params.Timeout, params.LogFile)
			}
			return ""
		}(),
		params.LogFile,
		timeoutMonitor,
		cdPrefix, params.PidFile, escapedCmd, params.LogFile,
		params.LogFile,
		params.StatusFile, params.NotifyCmd)
}

// prepareWorkingDir replaces ~ with $HOME and quotes the path to handle spaces
// Example: "~/my project" -> "$HOME/my project" (with quotes)
// Returns empty string if dir is empty (job will run from home directory)
func prepareWorkingDir(dir string) string {
	if dir == "" {
		return ""
	}

	// Replace leading ~ or ~/ with $HOME
	if strings.HasPrefix(dir, "~/") {
		dir = "$HOME/" + dir[2:]
	} else if dir == "~" {
		dir = "$HOME"
	}

	// Quote the path to handle spaces and special characters
	// Use double quotes to allow $HOME expansion
	return fmt.Sprintf(`"%s"`, dir)
}

// escapeForBashC escapes a command for use in bash -c '...'
func escapeForBashC(s string) string {
	// Replace single quotes with '\'' (end quote, escaped quote, start quote)
	return strings.ReplaceAll(s, "'", `'\''`)
}
