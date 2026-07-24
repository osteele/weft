package campaign

import "github.com/osteele/weft/internal/db"

// hasActiveLaunchJobs returns true when at least one job has started on this
// instance and is still non-terminal.
//
// A superseded attempt is not active here: the job migrated to a newer attempt
// (e.g. `weft edit --retry` requeued it) and no longer runs on this launch.
// Counting it as active leaves the launch believed busy forever, so grace/idle
// reaping never fires and the now-jobless rental runs until its own time/spend
// cap.
func hasActiveLaunchJobs(jobs []*db.Job, outcomes map[int64]string) bool {
	for _, j := range jobs {
		if j == nil {
			continue
		}
		displayStatus := j.Status
		if outcomes != nil {
			displayStatus = AttemptDisplayStatus(j, outcomes)
		}
		if displayStatus == db.AttemptOutcomeSuperseded {
			continue
		}
		if jobStartedOnInstance(j) && !IsJobTerminal(displayStatus) {
			return true
		}
	}
	return false
}
