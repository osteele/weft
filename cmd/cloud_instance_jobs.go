package cmd

import "github.com/osteele/weft/internal/db"

type cloudInstanceJobGroups struct {
	current    []*db.Job
	historical []*db.Job
}

func groupCloudInstanceJobs(instanceID int64, jobs []*db.Job) cloudInstanceJobGroups {
	groups := cloudInstanceJobGroups{
		current:    make([]*db.Job, 0, len(jobs)),
		historical: make([]*db.Job, 0, len(jobs)),
	}
	for _, job := range jobs {
		if job == nil {
			continue
		}
		if job.CloudInstanceID != nil && *job.CloudInstanceID == instanceID {
			groups.current = append(groups.current, job)
			continue
		}
		groups.historical = append(groups.historical, job)
	}
	return groups
}
