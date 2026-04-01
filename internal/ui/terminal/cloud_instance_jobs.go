package terminal

import "github.com/osteele/weft/internal/db"

const historicalLaunchJobsHeader = "  Previous attempts on this instance:"

type cloudInstanceJobGroups struct {
	current    []*db.Job
	historical []*db.Job
}

func groupLaunchJobs(instanceID int64, jobs []*db.Job) cloudInstanceJobGroups {
	groups := cloudInstanceJobGroups{
		current:    make([]*db.Job, 0, len(jobs)),
		historical: make([]*db.Job, 0, len(jobs)),
	}
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if job.LaunchID != nil && *job.LaunchID == instanceID {
			groups.current = append(groups.current, job)
			continue
		}
		groups.historical = append(groups.historical, job)
	}
	return groups
}
