package skypilot

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
)

// CancelBinding cancels a managed job only after a fresh queue observation
// proves the selected binding is the sole task under that job ID. SkyPilot's
// cancel API is job-scoped, so unknown or sibling task membership must fail
// closed instead of broadening a task-qualified Weft action.
func (c Client) CancelBinding(ctx context.Context, database *sql.DB, binding *db.ExternalJobBinding) (bool, error) {
	if binding == nil {
		return false, fmt.Errorf("external binding is required")
	}
	jobs, err := c.ListJobs(ctx)
	if err != nil {
		return false, fmt.Errorf("confirm SkyPilot cancel scope: %w", err)
	}
	var matches []Job
	for _, job := range jobs {
		if strings.TrimSpace(job.ID) == strings.TrimSpace(binding.ExternalJobID) {
			matches = append(matches, job)
		}
	}
	if len(matches) == 0 {
		return false, fmt.Errorf("cannot confirm SkyPilot cancel scope for job %s", binding.ExternalJobID)
	}
	if len(matches) != 1 {
		return false, fmt.Errorf("SkyPilot job %s has %d visible tasks; refusing job-wide cancellation from one Weft job", binding.ExternalJobID, len(matches))
	}
	if task := strings.TrimSpace(binding.ExternalTaskID); task != "" && strings.TrimSpace(matches[0].TaskID) != task {
		return false, fmt.Errorf("cannot confirm SkyPilot task %s/%s for cancellation", binding.ExternalJobID, task)
	}
	if db.IsTerminalStatus(NormalizeStatus(matches[0].Status)) {
		obs := ObservationFromJob(matches[0], binding.SubmittedFromProject, binding.SubmittedFromWorkingDir)
		if _, err := db.ApplyExternalJobObservation(database, binding.JobID, obs); err != nil {
			return false, fmt.Errorf("record terminal SkyPilot observation: %w", err)
		}
		return false, nil
	}
	if err := c.Cancel(ctx, binding.ExternalJobID); err != nil {
		return false, err
	}
	return db.SetExternalCancelIntent(database, binding.JobID)
}
