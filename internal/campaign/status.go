package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/cloud"
	"github.com/osteele/weft/internal/db"
)

// InstanceUpdate is a snapshot of cloud instance + job state.
type InstanceUpdate struct {
	CloudInstance *db.CloudInstance
	Jobs          []*db.Job
	Instance      *cloud.Instance // nil if not yet provisioned
}

// WatchInstance polls DB and cloud provider, sends updates on the returned channel.
// Closes the channel when the instance reaches a terminal state or ctx is cancelled.
func WatchInstance(ctx context.Context, client cloud.Client, database *sql.DB, cloudInstanceID int64, dbInterval, providerInterval time.Duration) <-chan InstanceUpdate {
	ch := make(chan InstanceUpdate, 1)

	go func() {
		defer close(ch)
		var lastProviderPoll time.Time
		var cachedInstance *cloud.Instance

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			ci, err := db.GetCloudInstance(database, cloudInstanceID)
			if err != nil || ci == nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(dbInterval):
					continue
				}
			}

			jobs, _ := db.GetCloudInstanceJobs(database, cloudInstanceID)

			// Refresh cloud instance info periodically
			providerInstID := ci.EffectiveProviderID()
			if providerInstID != "" && time.Since(lastProviderPoll) >= providerInterval {
				inst, showErr := client.ShowInstance(providerInstID)
				if showErr == nil {
					cachedInstance = inst
				}
				lastProviderPoll = time.Now()

				// Detect dead instances: provider says dead but DB says running
				if isProviderTerminal(inst) && !IsInstanceTerminal(ci.Status) {
					_ = db.UpdateCloudInstanceStatus(database, cloudInstanceID, db.CloudInstanceStatusFailed)
					_, _ = db.ResetCloudInstanceJobs(database, cloudInstanceID, db.AttemptOutcomeOrphaned)
					ci.Status = db.CloudInstanceStatusFailed
				}
			}

			update := InstanceUpdate{
				CloudInstance: ci,
				Jobs:          jobs,
				Instance:      cachedInstance,
			}

			select {
			case ch <- update:
			case <-ctx.Done():
				return
			}

			// Check for terminal state
			if IsInstanceTerminal(ci.Status) {
				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(dbInterval):
			}
		}
	}()

	return ch
}

// FormatPlainUpdate returns line-oriented output for an instance state change.
// Only returns lines for fields that changed between prev and curr.
// If prev is nil, all fields are reported.
func FormatPlainUpdate(prev, curr InstanceUpdate) string {
	var lines []string
	id := curr.CloudInstance.ID

	if prev.CloudInstance == nil || prev.CloudInstance.Status != curr.CloudInstance.Status {
		line := fmt.Sprintf("instance %d: status=%s", id, curr.CloudInstance.Status)
		providerInstID := curr.CloudInstance.EffectiveProviderID()
		if providerInstID != "" {
			line += fmt.Sprintf(" provider_id=%s", providerInstID)
		}
		if curr.Instance != nil && curr.Instance.SSHHost != "" {
			line += fmt.Sprintf(" ssh=\"%s\"", FormatSSHCommand(curr.Instance))
		}
		lines = append(lines, line)

		// Show grace period help when entering grace status
		if curr.CloudInstance.Status == db.CloudInstanceStatusGrace {
			if curr.CloudInstance.GraceDeadline != nil {
				deadline := time.Unix(*curr.CloudInstance.GraceDeadline, 0)
				remaining := time.Until(deadline).Truncate(time.Second)
				lines = append(lines, fmt.Sprintf("instance %d: grace period — %s remaining (until %s)",
					id, remaining, deadline.Format("15:04")))
			}
			// Show actionable commands for failed jobs
			for _, j := range curr.Jobs {
				if j.Status == db.StatusFailed {
					lines = append(lines, "")
					lines = append(lines, fmt.Sprintf("  To resubmit job %d with updated sources:", j.ID))
					lines = append(lines, fmt.Sprintf("    weft instance submit %d %d", id, j.ID))
					lines = append(lines, "  To resubmit with a modified command:")
					lines = append(lines, fmt.Sprintf("    weft instance submit %d %d --command '...'", id, j.ID))
				}
			}
			lines = append(lines, "  To extend the grace period:")
			lines = append(lines, fmt.Sprintf("    weft instance extend %d 15m", id))
			lines = append(lines, "  To release the instance:")
			lines = append(lines, fmt.Sprintf("    weft instance release %d", id))
		}
	}

	// Report job status changes
	prevJobStatus := make(map[int64]string)
	for _, j := range prev.Jobs {
		prevJobStatus[j.ID] = j.Status
	}
	for _, j := range curr.Jobs {
		if prevJobStatus[j.ID] != j.Status {
			line := fmt.Sprintf("instance %d: job %d status=%s", id, j.ID, j.Status)
			if j.ExitCode != nil {
				line += fmt.Sprintf(" exit=%d", *j.ExitCode)
			}
			lines = append(lines, line)
		}
	}

	return strings.Join(lines, "\n")
}

// IsInstanceTerminal returns true if the instance status is a terminal state.
// Note: "grace" is NOT terminal — the instance is still alive waiting for resubmission.
func IsInstanceTerminal(status string) bool {
	return status == db.CloudInstanceStatusCompleted ||
		status == db.CloudInstanceStatusFailed ||
		status == db.CloudInstanceStatusCancelled
}
