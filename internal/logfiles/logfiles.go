package logfiles

import (
	"fmt"
	"strings"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/session"
	"github.com/osteele/remote-jobs/internal/ssh"
)

// Location describes where a job's log lives on the remote host.
type Location struct {
	Path      string
	IsPattern bool
}

// LocationForJob returns the base location for a job's log along with whether it is a glob pattern.
func LocationForJob(job *db.Job) Location {
	if job == nil {
		return Location{}
	}
	path, isPattern := session.JobLogPath(job.ID, job.StartTime, job.SessionName)
	return Location{Path: path, IsPattern: isPattern}
}

// Resolve attempts to find an actual log file path for the job. For timestamped jobs this is
// already concrete; for queue-runner jobs it attempts to expand the glob on the remote host.
// The returned boolean indicates whether the path is a confirmed file (true) or a best-effort
// fallback (false).
func Resolve(job *db.Job) (string, bool) {
	loc := LocationForJob(job)
	if job == nil {
		return "", false
	}
	if !loc.IsPattern {
		return loc.Path, true
	}

	findCmd := fmt.Sprintf("ls -t %s 2>/dev/null | head -1", loc.Path)
	stdout, _, err := ssh.Run(job.Host, findCmd)
	if err == nil {
		trimmed := strings.TrimSpace(stdout)
		if trimmed != "" {
			return trimmed, true
		}
	}

	// Fall back to the simple path, even though it may not exist (matches previous behavior)
	return session.SimpleLogFile(job.ID), false
}
