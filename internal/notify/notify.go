// Package notify delivers durable job lifecycle events to user-configured
// local hooks. Structured hooks receive versioned JSON and acknowledge each
// event independently; the [notifications] command remains a terminal-event
// compatibility hook with WEFT_JOB_* environment variables.
//
// WEFT_JOB_SUMMARY is the broadcast-safe completion fact. The separately
// exported WEFT_JOB_SESSION_NOTE is scoped to the submitting session and must
// not be broadcast to other project participants.
//
// Callers invoke JobTerminal only after committing a terminal transition.
// Delivery failures are persisted for retry and never affect job-state
// progression. At-least-once delivery means hook consumers must be idempotent.
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

// JobTerminal drains the durable lifecycle-event outbox after a caller has
// committed a terminal job transition. The transition itself creates the event
// through the database trigger; finalStatus and exitCode remain in the signature
// so existing transition call sites do not have to reconstruct notification
// policy.
func JobTerminal(database *sql.DB, jobID int64, finalStatus string, exitCode *int) {
	if err := db.EnsureJobTerminalEvent(database, jobID, finalStatus, time.Now()); err != nil {
		slog.Warn("notify: ensure terminal event", "component", "notify", "job_id", jobID, "error", err)
		return
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Warn("notify: load config", "component", "notify", "job_id", jobID, "error", err)
		return
	}
	_ = exitCode
	dispatchLifecycleEvents(database, cfg, jobID)
}

// Run executes the legacy notify command synchronously with WEFT_JOB_* env
// vars. Exposed separately from JobTerminal for compatibility tests.
func Run(command string, job *db.Job, finalStatus string, exitCode *int, submitterSession string) {
	if err := run(command, job, finalStatus, exitCode, submitterSession, ""); err != nil {
		slog.Warn("notify: command failed", "component", "notify", "job_id", job.ID, "error", err)
	}
}

func run(command string, job *db.Job, finalStatus string, exitCode *int, submitterSession, sessionNote string) error {
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
		"WEFT_JOB_SESSION_NOTE="+sessionNote,
		"WEFT_JOB_SUBMITTER_SESSION="+submitterSession,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, string(out))
	}
	return nil
}

func buildSummary(job *db.Job, finalStatus string, exitCode *int) string {
	verb := finalStatus
	if exitCode != nil && *exitCode != 0 {
		verb = fmt.Sprintf("%s (exit %d)", finalStatus, *exitCode)
	}
	return fmt.Sprintf("Job %s %s", ids.FormatJobID(job.ID), verb)
}
