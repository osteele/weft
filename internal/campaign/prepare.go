package campaign

import (
	"database/sql"
	"log/slog"
	"sort"

	"github.com/osteele/weft/internal/config"
	"github.com/osteele/weft/internal/dataloc"
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/r2"
)

// bidLossEscalationThreshold is the number of consecutive interruptible
// launches a job may lose (orphaned or preempted attempts) before the next
// launch for its group searches on-demand offers instead of placing another
// bid. Motivated by the wj3741 incident: the job was orphaned 16 consecutive
// times on bid instances that kept getting outbid, with no escape hatch.
// Escalation is per-launch-attempt — a successful on-demand run breaks the
// consecutive chain, so subsequent launches may bid again.
const bidLossEscalationThreshold = 3

// ApplyBidLossEscalation marks groups whose preemptible jobs have repeatedly
// lost interruptible instances so the next offer search goes on-demand (see
// bidLossEscalationThreshold and offerConstraintsForGroup). It only ever
// reduces the interruptible opt-in; groups without preemptible jobs are
// untouched, and on-demand is never converted to a bid.
func ApplyBidLossEscalation(database *sql.DB, groups []InstanceGroup) {
	if database == nil {
		return
	}
	for i := range groups {
		g := &groups[i]
		if !g.HasPreemptibleJob() {
			continue
		}
		for _, job := range g.Jobs {
			if job == nil || !job.UsesPreemptiblePlacement() {
				continue
			}
			n, err := db.ConsecutiveInterruptibleOrphans(database, job.ID)
			if err != nil {
				slog.Warn("bid-loss escalation check failed", "component", "campaign", "job", job.ID, "error", err)
				continue
			}
			if n >= bidLossEscalationThreshold {
				g.EscalateToOnDemand = true
				slog.Info(
					"bid-loss escalation: consecutive bid instances lost — searching on-demand",
					"component", "campaign", "job", job.ID, "consecutive_losses", n,
					"threshold", bidLossEscalationThreshold,
				)
				break
			}
		}
	}
}

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
	ApplyBidLossEscalation(database, groups)
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
