package skypilot

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/ids"
)

// SyncBindings refreshes mirrored SkyPilot jobs. Explicit callers receive a
// command-level not-configured error; ambient callers use SyncBindingsAmbient.
func (c Client) SyncBindings(ctx context.Context, database *sql.DB, project string) (updated int, missing int, err error) {
	return c.syncBindings(ctx, database, project, nil, true)
}

// SyncBindingsAmbient refreshes mirrored jobs when SkyPilot is available and
// otherwise stays silent and mutation-free.
func (c Client) SyncBindingsAmbient(ctx context.Context, database *sql.DB, project string) (updated int, missing int, err error) {
	return c.syncBindings(ctx, database, project, nil, false)
}

// SyncJobBindingsAmbient refreshes only the selected mirrored jobs. An absent
// optional SkyPilot installation stays quiet and mutation-free.
func (c Client) SyncJobBindingsAmbient(ctx context.Context, database *sql.DB, jobIDs []int64) (updated int, missing int, err error) {
	selected := make(map[int64]struct{}, len(jobIDs))
	for _, jobID := range jobIDs {
		selected[jobID] = struct{}{}
	}
	return c.syncBindings(ctx, database, "", selected, false)
}

func (c Client) syncBindings(ctx context.Context, database *sql.DB, project string, selected map[int64]struct{}, explicit bool) (updated int, missing int, err error) {
	bindings, err := db.ListExternalJobBindings(database, db.ExternalExecutorSkyPilot, project)
	if err != nil {
		return 0, 0, fmt.Errorf("list external bindings: %w", err)
	}
	if selected != nil {
		filtered := bindings[:0]
		for _, binding := range bindings {
			if _, ok := selected[binding.JobID]; ok {
				filtered = append(filtered, binding)
			}
		}
		bindings = filtered
	}
	if len(bindings) == 0 {
		return 0, 0, nil
	}
	if !c.IsConfigured() {
		if explicit {
			return 0, 0, ErrNotConfigured
		}
		return 0, 0, nil
	}
	jobs, err := c.ListJobs(ctx)
	if err != nil {
		for _, binding := range bindings {
			if markErr := db.MarkExternalSyncWarning(database, binding.JobID, "SkyPilot sync failed: "+err.Error()); markErr != nil {
				return 0, 0, fmt.Errorf("record stale SkyPilot observation for %s: %w", ids.FormatJobID(binding.JobID), markErr)
			}
		}
		return 0, 0, fmt.Errorf("query SkyPilot jobs: %w", err)
	}
	for _, binding := range bindings {
		job, ok, warning := findJobForBinding(jobs, binding)
		if !ok {
			if err := db.MarkExternalSyncWarning(database, binding.JobID, warning); err != nil {
				return 0, 0, fmt.Errorf("record stale SkyPilot observation for %s: %w", ids.FormatJobID(binding.JobID), err)
			}
			missing++
			continue
		}
		if strings.TrimSpace(job.Status) == "" {
			if err := db.MarkExternalSyncWarning(database, binding.JobID, "SkyPilot returned the job without a status"); err != nil {
				return 0, 0, fmt.Errorf("record inconclusive SkyPilot observation for %s: %w", ids.FormatJobID(binding.JobID), err)
			}
			continue
		}
		obs := ObservationFromJob(job, binding.SubmittedFromProject, binding.SubmittedFromWorkingDir)
		applied, err := db.ApplyExternalJobObservation(database, binding.JobID, obs)
		if err != nil {
			return 0, 0, fmt.Errorf("update %s: %w", ids.FormatJobID(binding.JobID), err)
		}
		if applied {
			updated++
		}
	}
	return updated, missing, nil
}

func findJobForBinding(jobs []Job, binding *db.ExternalJobBinding) (Job, bool, string) {
	externalID := strings.TrimSpace(binding.ExternalJobID)
	taskID := strings.TrimSpace(binding.ExternalTaskID)
	var matches []Job
	if taskID != "" {
		for _, job := range jobs {
			if strings.TrimSpace(job.TaskID) == taskID && strings.TrimSpace(job.ID) == externalID {
				matches = append(matches, job)
			}
		}
	} else {
		for _, job := range jobs {
			if strings.TrimSpace(job.ID) == externalID {
				matches = append(matches, job)
			}
		}
		if len(matches) == 0 {
			for _, job := range jobs {
				if strings.TrimSpace(job.Name) == externalID {
					matches = append(matches, job)
				}
			}
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], true, ""
	case 0:
		return Job{}, false, "SkyPilot job was not returned by sky jobs queue"
	default:
		return Job{}, false, "SkyPilot identity matched multiple jobs in sky jobs queue"
	}
}
