// Package notify runs a user-configured local command when a job reaches a
// terminal status. The command is configured via the [notifications] section
// of config.toml and receives job context through WEFT_JOB_* environment
// variables. The intended use is pushing job-completion events to local
// agents (e.g. `agent-mail notify`), but the mechanism is generic.
//
// Callers invoke JobTerminal only at transition points (after a
// transition-validated DB update succeeds), so each terminal transition
// notifies at most once. Notification failures are logged, never fatal:
// a broken notify command must not affect sync.
package notify

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// commandTimeout bounds the notify command so a hung notifier cannot stall
// sync. Local delivery (agent-mail) completes in well under a second.
const commandTimeout = 10 * time.Second

// JobTerminal notifies the configured command that jobID reached
// finalStatus (db.StatusCompleted or db.StatusFailed). exitCode may be nil
// when unknown. No-op when no command is configured.
func JobTerminal(database *sql.DB, jobID int64, finalStatus string, exitCode *int) {
	cfg, err := config.Load()
	if err != nil || cfg.Notifications.Command == "" {
		return
	}
	job, err := db.GetJobByID(database, jobID)
	if err != nil || job == nil {
		slog.Warn("notify: job lookup failed", "component", "notify", "job_id", jobID, "error", err)
		return
	}
	Run(cfg.Notifications.Command, job, finalStatus, exitCode)
}

// Run executes the notify command synchronously with WEFT_JOB_* env vars.
// Exposed separately from JobTerminal for testing.
func Run(command string, job *db.Job, finalStatus string, exitCode *int) {
	exitStr := ""
	if exitCode != nil {
		exitStr = fmt.Sprintf("%d", *exitCode)
	}
	summary := buildSummary(job, finalStatus, exitCode)

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = append(os.Environ(),
		"WEFT_JOB_ID="+ids.FormatJobID(job.ID),
		"WEFT_JOB_STATUS="+finalStatus,
		"WEFT_JOB_EXIT_CODE="+exitStr,
		"WEFT_JOB_DIR="+job.WorkingDir,
		"WEFT_JOB_DESCRIPTION="+job.Description,
		"WEFT_JOB_HOST="+job.Host,
		"WEFT_JOB_SUMMARY="+summary,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Warn("notify: command failed", "component", "notify",
			"job_id", job.ID, "error", err, "output", string(out))
	}
}

func buildSummary(job *db.Job, finalStatus string, exitCode *int) string {
	verb := finalStatus
	if exitCode != nil && *exitCode != 0 {
		verb = fmt.Sprintf("%s (exit %d)", finalStatus, *exitCode)
	}
	desc := job.Description
	if desc == "" {
		desc = job.Command
	}
	if desc != "" {
		return fmt.Sprintf("Job %s %s on %s: %s",
			ids.FormatJobID(job.ID), verb, job.Host, desc)
	}
	return fmt.Sprintf("Job %s %s on %s", ids.FormatJobID(job.ID), verb, job.Host)
}
