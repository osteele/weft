package campaign

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/vastai"
)

// InstanceUpdate is a snapshot of cloud instance + job state.
type InstanceUpdate struct {
	CloudInstance *db.CloudInstance
	Jobs          []*db.Job
	Instance      *vastai.Instance // nil if not yet provisioned
}

// WatchInstance polls DB and Vast.ai, sends updates on the returned channel.
// Closes the channel when the instance reaches a terminal state or ctx is cancelled.
func WatchInstance(ctx context.Context, client vastai.VastaiClient, database *sql.DB, cloudInstanceID int64, dbInterval, vastaiInterval time.Duration) <-chan InstanceUpdate {
	ch := make(chan InstanceUpdate, 1)

	go func() {
		defer close(ch)
		var lastVastaiPoll time.Time
		var cachedInstance *vastai.Instance

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

			// Refresh Vast.ai instance info periodically
			if ci.VastaiInstanceID != "" && time.Since(lastVastaiPoll) >= vastaiInterval {
				var instID int
				if _, err := fmt.Sscanf(ci.VastaiInstanceID, "%d", &instID); err == nil {
					if inst, err := client.ShowInstance(instID); err == nil {
						cachedInstance = inst
					}
				}
				lastVastaiPoll = time.Now()
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
		if curr.CloudInstance.VastaiInstanceID != "" {
			line += fmt.Sprintf(" vastai=%s", curr.CloudInstance.VastaiInstanceID)
		}
		if curr.Instance != nil && curr.Instance.SSHHost != "" {
			line += fmt.Sprintf(" ssh=\"%s\"", FormatSSHCommand(curr.Instance))
		}
		lines = append(lines, line)
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
func IsInstanceTerminal(status string) bool {
	return status == db.CloudInstanceStatusCompleted ||
		status == db.CloudInstanceStatusFailed ||
		status == db.CloudInstanceStatusCancelled
}
