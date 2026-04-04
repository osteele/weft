package campaign

import "github.com/osteele/weft/internal/db"

// hasActiveLaunchJobs returns true when at least one job has started on this
// instance and is still non-terminal.
func hasActiveLaunchJobs(jobs []*db.Job, outcomes map[int64]string) bool {
	for _, j := range jobs {
		if j == nil {
			continue
		}
		displayStatus := j.Status
		if outcomes != nil {
			displayStatus = AttemptDisplayStatus(j, outcomes)
		}
		if jobStartedOnInstance(j, displayStatus) && !IsJobTerminal(displayStatus) {
			return true
		}
	}
	return false
}
