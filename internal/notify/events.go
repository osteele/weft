package notify

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/sessioninbox"
)

const (
	HookAckSchemaVersion     = "weft-hook-ack/v1"
	legacyNotificationHookID = "notifications/legacy"
	agentReviewSessionPrefix = "agent-review-daemon/"
	deliveryLease            = 30 * time.Second
	maxDeliveryBatch         = 64
	maxHookOutputBytes       = 64 << 10
)

type eventHook struct {
	id      string
	command []string
	legacy  string
}

type hookAcknowledgement struct {
	SchemaVersion     string `json:"schema_version"`
	Disposition       string `json:"disposition"`
	Detail            string `json:"detail,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

// DispatchLifecycleEvents delivers every due event independently to each
// configured hook.
func DispatchLifecycleEvents(database *sql.DB, cfg *config.Config) {
	dispatchLifecycleEvents(database, cfg, 0)
}

// dispatchLifecycleEvents preserves the legacy notification seam's one-job
// scope while generic hooks drain the complete durable backlog.
func dispatchLifecycleEvents(database *sql.DB, cfg *config.Config, legacyJobID int64) {
	for _, hook := range configuredHooks(cfg) {
		claimJobID := int64(0)
		if hook.legacy != "" {
			claimJobID = legacyJobID
		}
		for range maxDeliveryBatch {
			now := time.Now()
			deliveries, err := db.ClaimLifecycleHookDeliveries(database, hook.id, legacyJobID, claimJobID, now, deliveryLease, 1)
			if err != nil {
				slog.Warn("notify: claim lifecycle hook deliveries", "component", "notify", "hook_id", hook.id, "error", err)
				break
			}
			if len(deliveries) == 0 {
				break
			}
			delivery := deliveries[0]
			disposition, detail, retryAfter := deliverLifecycleEvent(database, hook, delivery)
			finishedAt := time.Now()
			if disposition == "retry" {
				if retryAfter <= 0 {
					retryAfter = deliveryBackoff(delivery.Attempts)
				}
				if _, err := db.RecordLifecycleHookRetry(database, delivery, detail, finishedAt, retryAfter); err != nil {
					slog.Warn("notify: record lifecycle hook retry", "component", "notify", "hook_id", hook.id, "event_id", delivery.Event.EventID, "error", err)
				}
				continue
			}
			if _, err := db.FinishLifecycleHookDelivery(database, delivery.Event.EventID, hook.id, delivery.LeaseToken, disposition, detail, finishedAt, 0); err != nil {
				slog.Warn("notify: finish lifecycle hook delivery", "component", "notify", "hook_id", hook.id, "event_id", delivery.Event.EventID, "error", err)
			}
		}
	}
}

func configuredHooks(cfg *config.Config) []eventHook {
	if cfg == nil {
		return nil
	}
	seen := make(map[string]struct{}, len(cfg.Events.Hooks)+1)
	hooks := make([]eventHook, 0, len(cfg.Events.Hooks)+1)
	if command := strings.TrimSpace(cfg.Notifications.Command); command != "" {
		seen[legacyNotificationHookID] = struct{}{}
		hooks = append(hooks, eventHook{id: legacyNotificationHookID, legacy: command})
	}
	for _, configured := range cfg.Events.Hooks {
		id := strings.TrimSpace(configured.ID)
		if id == "" {
			slog.Warn("notify: ignoring lifecycle hook with empty id", "component", "notify")
			continue
		}
		if id == legacyNotificationHookID {
			slog.Warn("notify: lifecycle hook uses reserved id", "component", "notify", "hook_id", id)
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			slog.Warn("notify: duplicate lifecycle hook id", "component", "notify", "hook_id", id)
			continue
		}
		if len(configured.Command) == 0 || strings.TrimSpace(configured.Command[0]) == "" {
			slog.Warn("notify: ignoring lifecycle hook with empty command", "component", "notify", "hook_id", id)
			continue
		}
		command := append([]string(nil), configured.Command...)
		command[0] = strings.TrimSpace(command[0])
		seen[id] = struct{}{}
		hooks = append(hooks, eventHook{id: id, command: command})
	}
	return hooks
}

func deliverLifecycleEvent(database *sql.DB, hook eventHook, delivery db.LifecycleHookDelivery) (string, string, time.Duration) {
	if hook.legacy != "" {
		if strings.HasPrefix(delivery.Event.SubmitterSession, agentReviewSessionPrefix) {
			return "ignored", "reserved agent-review submitter session", 0
		}
		job, err := db.GetJobByID(database, delivery.Event.JobNumericID)
		if err != nil || job == nil {
			return "retry", fmt.Sprintf("load job: %v", err), 0
		}
		// The compatibility hook remains a job-completion hook even though the
		// durable event substrate can carry other lifecycle event kinds.
		if delivery.Event.EventKind != "job.terminal" || job.LatestRunID == nil || *job.LatestRunID != delivery.Event.AttemptID {
			return "ignored", "not the authoritative terminal attempt", 0
		}
		query, err := sessioninbox.Load(database, job.Project, delivery.Event.SubmitterSession, time.Now())
		if err != nil {
			slog.Warn("notify: unprocessed session inbox lookup failed", "component", "notify", "job_id", job.ID, "error", err)
		}
		if err := run(hook.legacy, job, delivery.Event.Status, job.ExitCode, delivery.Event.SubmitterSession, sessioninbox.FormatReminder(query)); err != nil {
			return "retry", err.Error(), 0
		}
		return "handled", "", 0
	}

	payload, err := json.Marshal(delivery.Event)
	if err != nil {
		return "rejected", fmt.Sprintf("encode event: %v", err), 0
	}
	payload = append(payload, '\n')
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, hook.command[0], hook.command[1:]...)
	command.Env = append(os.Environ(),
		"WEFT_EVENT_ID="+delivery.Event.EventID,
		"WEFT_JOB_ID="+delivery.Event.JobID,
		"WEFT_JOB_SUBMITTER_SESSION="+delivery.Event.SubmitterSession,
	)
	command.Stdin = bytes.NewReader(payload)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &limitedWriter{writer: &stdout, remaining: maxHookOutputBytes}
	command.Stderr = &limitedWriter{writer: &stderr, remaining: maxHookOutputBytes}
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		return "retry", fmt.Sprintf("hook command: %v: %s", err, detail), 0
	}
	ack, err := decodeHookAcknowledgement(stdout.Bytes())
	if err != nil {
		return "retry", err.Error(), 0
	}
	retryAfter := time.Duration(ack.RetryAfterSeconds) * time.Second
	if retryAfter > 5*time.Minute {
		retryAfter = 5 * time.Minute
	}
	return ack.Disposition, ack.Detail, retryAfter
}

func decodeHookAcknowledgement(data []byte) (hookAcknowledgement, error) {
	var ack hookAcknowledgement
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&ack); err != nil {
		return ack, fmt.Errorf("decode hook acknowledgement: %w", err)
	}
	var extra objectSentinel
	if err := decoder.Decode(&extra); err != io.EOF {
		return ack, fmt.Errorf("decode hook acknowledgement: trailing data")
	}
	if ack.SchemaVersion != HookAckSchemaVersion {
		return ack, fmt.Errorf("hook acknowledgement schema %q, want %q", ack.SchemaVersion, HookAckSchemaVersion)
	}
	switch ack.Disposition {
	case "handled", "ignored", "retry", "rejected":
	default:
		return ack, fmt.Errorf("hook acknowledgement disposition %q is invalid", ack.Disposition)
	}
	if ack.RetryAfterSeconds < 0 {
		return ack, fmt.Errorf("hook acknowledgement retry_after_seconds must be nonnegative")
	}
	return ack, nil
}

type objectSentinel struct{}

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *limitedWriter) Write(data []byte) (int, error) {
	original := len(data)
	if w.remaining > 0 {
		chunk := data
		if len(chunk) > w.remaining {
			chunk = chunk[:w.remaining]
		}
		_, _ = w.writer.Write(chunk)
		w.remaining -= len(chunk)
	}
	return original, nil
}

func deliveryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 8 {
		attempt = 8
	}
	return time.Duration(1<<(attempt-1)) * time.Second
}
