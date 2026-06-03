package campaign

import (
	"database/sql"
	"sort"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

// PrepareGroups builds launch-ready instance groups from a list of jobs:
// groups by GPU affinity, filters by GPU class, splits by Docker image,
// and estimates disk needs.
func PrepareGroups(jobs []*db.Job, database *sql.DB, gpuFilter string, r2Client *r2.Client) []InstanceGroup {
	return PrepareGroupsWithConfig(jobs, database, nil, gpuFilter, r2Client)
}

func PrepareGroupsWithConfig(jobs []*db.Job, database *sql.DB, cfg *config.Config, gpuFilter string, r2Client *r2.Client) []InstanceGroup {
	groups := GroupByAffinity(jobs, dataloc.LookupCachedModelSize)
	groups = FilterByGPUClass(groups, gpuFilter)
	groups = SplitGroupsByImage(database, groups)
	groups = ApplyImageMetadataRequirements(cfg, groups)
	var diskAnomalies []DiskTelemetryAnomaly
	for i := range groups {
		var anomalies []DiskTelemetryAnomaly
		groups[i].DiskGB, anomalies = EstimateGroupDisk(groups[i], database, r2Client)
		diskAnomalies = append(diskAnomalies, anomalies...)
	}
	if len(diskAnomalies) > 0 {
		recordDiskTelemetryAnomalies(database, groups, diskAnomalies)
	}
	return groups
}

// FlattenGroupJobs collects all jobs from the given groups, sorted by ID.
func FlattenGroupJobs(groups []InstanceGroup) []*db.Job {
	var jobs []*db.Job
	for _, g := range groups {
		jobs = append(jobs, g.Jobs...)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].ID < jobs[j].ID })
	return jobs
}
