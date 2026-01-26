package ops

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/osteele/remote-jobs/internal/artifacts"
	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

type slurmInfo struct {
	State         string
	ExitCode      *int
	StartTime     *int64
	EndTime       *int64
	FailureReason string
}

func submitSlurmJob(database *sql.DB, job *db.Job, timeout time.Duration) (string, string, error) {
	if timeout == 0 {
		timeout = 30 * time.Second
	}

	envVars := artifacts.MergeEnvVars(job.EnvVars, job.ID)
	script := buildSlurmScript(job, envVars)
	wrap := fmt.Sprintf("bash -lc %s", shellQuote(script))

	logFile := preparePath(session.SimpleLogFile(job.ID))
	if logFile == "" {
		return "", "", fmt.Errorf("log file path is empty")
	}
	chdir := preparePath(job.WorkingDir)
	chdirArg := ""
	if chdir != "" {
		chdirArg = fmt.Sprintf("--chdir %s", chdir)
	}

	cmd := fmt.Sprintf("sbatch --parsable --job-name %s --output %s --error %s %s --wrap %s",
		shellQuote(fmt.Sprintf("rj-%d", job.ID)),
		logFile,
		logFile,
		chdirArg,
		shellQuote(wrap),
	)
	stdout, stderr, err := ssh.RunWithTimeout(job.Host, cmd, timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return "", "", fmt.Errorf("connection error: %s", strings.TrimSpace(stderr))
		}
		return "", "", fmt.Errorf("sbatch failed: %s", strings.TrimSpace(stderr))
	}

	remoteID := parseSlurmJobID(stdout)
	if remoteID == "" {
		return "", "", fmt.Errorf("unable to parse slurm job id from %q", strings.TrimSpace(stdout))
	}

	if err := db.SetJobRemoteID(database, job.ID, remoteID); err != nil {
		return "", "", fmt.Errorf("set slurm job id: %w", err)
	}
	if err := db.SetJobRemoteState(database, job.ID, "PENDING", ""); err != nil {
		return "", "", fmt.Errorf("set slurm state: %w", err)
	}
	job.RemoteID = remoteID
	job.RemoteState = "PENDING"
	return remoteID, "PENDING", nil
}

func buildSlurmScript(job *db.Job, envVars []string) string {
	expandedStatus := expandHome(session.SimpleStatusFile(job.ID))
	var exports []string
	for _, ev := range envVars {
		if strings.TrimSpace(ev) == "" {
			continue
		}
		exports = append(exports, fmt.Sprintf("export %s", escapeForBash(ev)))
	}
	envPrefix := ""
	if len(exports) > 0 {
		envPrefix = strings.Join(exports, "; ") + "; "
	}

	cmd := fmt.Sprintf("%s%s", envPrefix, job.Command)
	cmd = strings.TrimSpace(cmd)

	lines := []string{}
	if cmd != "" {
		lines = append(lines, cmd)
	}
	lines = append(lines, fmt.Sprintf("EXIT_CODE=$?"))
	lines = append(lines, fmt.Sprintf("echo $EXIT_CODE > \"%s\"", expandedStatus))
	lines = append(lines, "exit $EXIT_CODE")
	return strings.Join(lines, "; ")
}

func probeSlurmInfo(job *db.Job, timeout time.Duration) (*slurmInfo, error) {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if job.RemoteID == "" {
		return nil, nil
	}

	// Try squeue for active jobs
	squeueCmd := fmt.Sprintf("squeue -h -j %s -o \"%%T|%%S|%%V\"", shellQuote(job.RemoteID))
	stdout, stderr, err := ssh.RunWithTimeout(job.Host, squeueCmd, timeout)
	if err != nil && ssh.IsConnectionError(stderr) {
		return nil, fmt.Errorf("connection error: %s", strings.TrimSpace(stderr))
	}
	if strings.TrimSpace(stdout) != "" {
		parts := strings.Split(strings.TrimSpace(stdout), "|")
		info := &slurmInfo{}
		if len(parts) > 0 {
			info.State = strings.TrimSpace(parts[0])
		}
		if len(parts) > 1 {
			if ts := parseSlurmTime(parts[1]); ts != nil {
				info.StartTime = ts
			}
		}
		return info, nil
	}

	// Fallback to sacct for historical jobs
	sacctCmd := fmt.Sprintf("sacct -n -P -j %s -o JobIDRaw,State,ExitCode,Start,End", shellQuote(job.RemoteID))
	stdout, stderr, err = ssh.RunWithTimeout(job.Host, sacctCmd, timeout)
	if err != nil && ssh.IsConnectionError(stderr) {
		return nil, fmt.Errorf("connection error: %s", strings.TrimSpace(stderr))
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) < 5 {
			continue
		}
		if strings.TrimSpace(fields[0]) != job.RemoteID {
			continue
		}
		info := &slurmInfo{
			State: strings.TrimSpace(fields[1]),
		}
		if exit := parseSlurmExitCode(fields[2]); exit != nil {
			info.ExitCode = exit
		}
		if ts := parseSlurmTime(fields[3]); ts != nil {
			info.StartTime = ts
		}
		if ts := parseSlurmTime(fields[4]); ts != nil {
			info.EndTime = ts
		}
		info.FailureReason = normalizeSlurmFailure(info.State)
		return info, nil
	}

	return nil, nil
}

// SyncSlurmJob checks and updates a SLURM job's status.
func SyncSlurmJob(database *sql.DB, job *db.Job, opts SyncOptions) (SyncResult, error) {
	timeout := effectiveSyncTimeout(opts.Timeout)
	info, err := probeSlurmInfo(job, timeout)
	if err != nil {
		// SSH error - host not contacted
		return SyncResult{}, err
	}
	// Host was contacted (even if no SLURM info found)
	if info == nil || info.State == "" {
		return SyncResult{HostContacted: true}, nil
	}
	if err := db.SetJobRemoteState(database, job.ID, info.State, info.FailureReason); err != nil {
		return SyncResult{HostContacted: true}, err
	}

	localStatus := slurmStateToLocal(job, info)
	switch localStatus {
	case db.StatusRunning, db.StatusQueued:
		if job.Status != localStatus {
			if err := db.UpdateStatusAndLastSynced(database, job.ID, localStatus); err != nil {
				return SyncResult{HostContacted: true}, err
			}
			return SyncResult{Updated: true, HostContacted: true}, nil
		}
		return SyncResult{HostContacted: true}, nil
	case db.StatusCompleted:
		exitCode := 0
		if info.ExitCode != nil {
			exitCode = *info.ExitCode
		}
		endTime := int64(0)
		if info.EndTime != nil {
			endTime = *info.EndTime
		}
		if err := RecordJobCompletion(database, job.ID, exitCode, endTime); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		CacheCompletedJobLog(job)
		return SyncResult{Updated: true, HostContacted: true}, nil
	case db.StatusKilled:
		if err := db.UpdateStatusAndLastSynced(database, job.ID, db.StatusKilled); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		return SyncResult{Updated: true, HostContacted: true}, nil
	case db.StatusFailed:
		if err := db.MarkDeadByID(database, job.ID); err != nil {
			return SyncResult{HostContacted: true}, err
		}
		return SyncResult{Updated: true, HostContacted: true}, nil
	default:
		return SyncResult{HostContacted: true}, nil
	}
}

func slurmStateToLocal(job *db.Job, info *slurmInfo) string {
	if info == nil {
		return job.Status
	}
	state := strings.ToUpper(strings.TrimSpace(info.State))
	switch state {
	case "PENDING", "CONFIGURING", "REQUEUED", "SUSPENDED":
		return db.StatusQueued
	case "RUNNING", "COMPLETING":
		return db.StatusRunning
	case "COMPLETED":
		return db.StatusCompleted
	case "CANCELLED", "CANCELLED+", "CANCELLED_BY_USER":
		return db.StatusKilled
	case "FAILED", "TIMEOUT", "NODE_FAIL", "PREEMPTED", "OUT_OF_MEMORY":
		return db.StatusFailed
	default:
		return job.Status
	}
}

func normalizeSlurmFailure(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "TIMEOUT":
		return "timeout"
	case "OUT_OF_MEMORY":
		return "oom"
	case "NODE_FAIL":
		return "node_fail"
	case "PREEMPTED":
		return "preempted"
	case "CANCELLED", "CANCELLED+", "CANCELLED_BY_USER":
		return "canceled"
	case "FAILED":
		return "failed"
	default:
		return ""
	}
}

func parseSlurmExitCode(raw string) *int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ":")
	code, err := strconv.Atoi(parts[0])
	if err != nil {
		return nil
	}
	return &code
}

func parseSlurmTime(raw string) *int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "Unknown" || raw == "None" {
		return nil
	}
	layouts := []string{
		"2006-01-02T15:04:05",
		"2006-01-02T15:04:05-07:00",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, raw); err == nil {
			sec := t.Unix()
			return &sec
		}
	}
	return nil
}

func parseSlurmJobID(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ";")
	return strings.TrimSpace(parts[0])
}

func expandHome(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "~/") {
		return "$HOME/" + path[2:]
	}
	if path == "~" {
		return "$HOME"
	}
	return path
}

func preparePath(path string) string {
	if path == "" {
		return ""
	}
	path = expandHome(path)
	path = strings.ReplaceAll(path, "\"", "\\\"")
	return fmt.Sprintf(`"%s"`, path)
}

func escapeForBash(s string) string {
	return strings.ReplaceAll(s, "'", `'\''`)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.ContainsAny(s, " \t\n\"'`$\\") {
		return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
	}
	return s
}

func cancelSlurmJob(job *db.Job, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	if job.RemoteID == "" {
		return nil
	}
	cmd := fmt.Sprintf("scancel %s", shellQuote(job.RemoteID))
	_, stderr, err := ssh.RunWithTimeout(job.Host, cmd, timeout)
	if err != nil {
		if ssh.IsConnectionError(stderr) {
			return fmt.Errorf("connection error: %s", strings.TrimSpace(stderr))
		}
		return fmt.Errorf("scancel failed: %s", strings.TrimSpace(stderr))
	}
	return nil
}
