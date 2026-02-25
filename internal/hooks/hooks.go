// Package hooks runs user-defined hook scripts in response to job lifecycle events.
package hooks

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/osteele/remote-jobs/internal/db"
	"github.com/osteele/remote-jobs/internal/oplog"
)

// hookTimeout is the maximum time a hook script may run before being killed.
const hookTimeout = 30 * time.Second

// hookStatuses are the terminal statuses that trigger the on-job-complete hook.
// Draft and canceled are excluded as they don't represent meaningful completion.
var hookStatuses = map[string]bool{
	db.StatusCompleted: true,
	db.StatusFailed:    true,
	db.StatusKilled:    true,
	db.StatusDead:      true,
}

// ShouldFireHook reports whether a status transition should fire the on-job-complete hook.
func ShouldFireHook(oldStatus, newStatus string) bool {
	if !hookStatuses[newStatus] {
		return false
	}
	// Don't fire if already in a terminal state (re-sync of same terminal status)
	return !db.IsTerminalStatus(oldStatus)
}

// hookPath returns the path to the on-job-complete hook script.
func hookPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "remote-jobs", "hooks", "on-job-complete")
}

// RunOnJobComplete invokes the on-job-complete hook if it exists and is executable.
// It runs asynchronously in a goroutine with a timeout and never blocks the caller.
func RunOnJobComplete(job *db.Job) {
	path := hookPath()
	if path == "" {
		return
	}

	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return
	}
	if info.Mode()&0111 == 0 {
		return
	}

	go runHook(path, job)
}

func runHook(path string, job *db.Job) {
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("JOB_ID=%d", job.ID),
		fmt.Sprintf("JOB_HOST=%s", job.Host),
		fmt.Sprintf("JOB_STATUS=%s", job.Status),
		fmt.Sprintf("JOB_DESCRIPTION=%s", job.EffectiveDescription()),
		fmt.Sprintf("JOB_DIR=%s", job.WorkingDir),
	)

	oplog.LogJob("hook.on-job-complete", job.ID, job.Host,
		oplog.WithDetailf("running hook: status=%s", job.Status))

	if err := cmd.Run(); err != nil {
		oplog.LogJob("hook.on-job-complete", job.ID, job.Host,
			oplog.WithDetailf("hook failed: status=%s", job.Status),
			oplog.WithError(err))
	}
}
