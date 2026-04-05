package terminal

import (
	"database/sql"
	"fmt"
	"sort"
	"time"

	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/queueblock"
)

const projectWatchSyncInterval = TerminalSyncInterval

func FilterProjectGroups(groups []ProjectGroup, project string) []ProjectGroup {
	return filterProjectGroups(groups, project)
}

func filterProjectGroups(groups []projectGroup, project string) []projectGroup {
	if project == "" {
		return groups
	}
	for _, group := range groups {
		if group.Label == project {
			return []projectGroup{group}
		}
	}
	return nil
}

func LoadProjectWatchGroups(database *sql.DB, recentWindow time.Duration) ([]ProjectGroup, error) {
	return loadProjectWatchGroups(database, recentWindow)
}

func loadProjectWatchGroups(database *sql.DB, recentWindow time.Duration) ([]projectGroup, error) {
	activeJobs := make([]*db.Job, 0)
	running, err := db.ListAllRunning(database)
	if err != nil {
		return nil, fmt.Errorf("list running jobs: %w", err)
	}
	activeJobs = append(activeJobs, running...)

	for _, status := range []string{db.StatusStarting, db.StatusPaused, db.StatusQueued, db.StatusPendingPlacement} {
		jobs, err := db.ListJobs(database, status, "", 0, nil, "")
		if err != nil {
			return nil, fmt.Errorf("list %s jobs: %w", status, err)
		}
		activeJobs = append(activeJobs, jobs...)
	}
	queueblock.Apply(activeJobs, queueblock.Fetch(activeJobs, 5*time.Second))

	recentJobs, err := db.ListRecentTerminalJobs(database, time.Now().Add(-recentWindow).Unix())
	if err != nil {
		return nil, fmt.Errorf("list recent terminal jobs: %w", err)
	}

	unplacedJobs, err := db.ListUnplacedJobs(database)
	if err != nil {
		return nil, fmt.Errorf("list unplaced jobs: %w", err)
	}
	unplacedSet := make(map[int64]bool, len(unplacedJobs))
	for _, job := range unplacedJobs {
		unplacedSet[job.ID] = true
	}

	groups := groupProjectActivity(activeJobs, recentJobs)
	classifyUnplacedJobs(groups, unplacedSet)
	if err := attachProjectLaunches(database, groups); err != nil {
		return nil, err
	}
	return groups, nil
}

func SyncProjectWatchData(database *sql.DB, fullSync bool, skipSync bool) []string {
	return syncProjectWatchData(database, fullSync, skipSync)
}

func syncProjectWatchData(database *sql.DB, fullSync bool, skipSync bool) []string {
	if skipSync {
		return nil
	}

	timeout := FastSyncTimeout
	if fullSync {
		timeout = DefaultSyncTimeout
	}
	completed, unreachable, warnings := performSyncWithTimeoutForHostsDetailed(database, nil, timeout, false)
	if !completed {
		if note := buildStaleDataNote(database, unreachable); note != "" {
			warnings = append(warnings, note)
		}
	}
	return warnings
}

func syncProjectWatchTUIData(database *sql.DB, full bool) []string {
	timeout := FastSyncTimeout
	if full {
		timeout = NormalSyncTimeout
	}
	completed, unreachable, warnings := performSyncWithTimeoutForHostsDetailedWithOptions(database, nil, timeout, false, full)
	if !completed {
		if note := buildStaleDataNote(database, unreachable); note != "" {
			warnings = append(warnings, note)
		}
	}
	warnings = append(warnings, syncCloudStateForTUI(database, full)...)
	return compactWarnings(warnings)
}

func classifyUnplacedJobs(groups []projectGroup, unplacedSet map[int64]bool) {
	for i := range groups {
		var queued []*db.Job
		for _, job := range groups[i].Queued {
			if unplacedSet[job.ID] {
				groups[i].Unplaced = append(groups[i].Unplaced, job)
			} else {
				queued = append(queued, job)
			}
		}
		groups[i].Queued = queued
	}
}

func attachProjectLaunches(database *sql.DB, groups []projectGroup) error {
	cache := make(map[int64]*db.Launch)
	for i := range groups {
		seen := make(map[int64]struct{})
		appendJobInstance := func(job *db.Job) error {
			if job == nil || job.LaunchID == nil || *job.LaunchID <= 0 {
				return nil
			}
			instanceID := *job.LaunchID
			if _, ok := seen[instanceID]; ok {
				return nil
			}
			seen[instanceID] = struct{}{}

			inst, ok := cache[instanceID]
			if !ok {
				var err error
				inst, err = db.GetLaunch(database, instanceID)
				if err != nil {
					return fmt.Errorf("get cloud instance %d: %w", instanceID, err)
				}
				cache[instanceID] = inst
			}
			if inst != nil {
				groups[i].CloudInsts = append(groups[i].CloudInsts, inst)
			}
			return nil
		}

		for _, bucket := range [][]*db.Job{groups[i].Running, groups[i].Queued, groups[i].Recent} {
			for _, job := range bucket {
				if err := appendJobInstance(job); err != nil {
					return err
				}
			}
		}
		sort.SliceStable(groups[i].CloudInsts, func(a, b int) bool {
			return groups[i].CloudInsts[a].ID < groups[i].CloudInsts[b].ID
		})
	}
	return nil
}
