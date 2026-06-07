package cmd

import (
	"database/sql"
	"time"

	"github.com/osteele/weft/internal/daemoncontrol"
	"github.com/osteele/weft/internal/db"
)

const daemonFreshSyncMaxAge = 2 * time.Minute

var daemonLiveFunc = daemonIsLive

func daemonIsLive() bool {
	status, err := daemoncontrol.CurrentStatus(daemoncontrol.DefaultPaths())
	return err == nil && status.Live
}

func liveSyncNeededForJobs(database *sql.DB, jobs []*db.Job, forceSync, noSync bool) bool {
	if noSync {
		return false
	}
	if forceSync {
		return true
	}
	if !daemonLiveFunc() {
		return true
	}
	now := time.Now()
	for _, job := range jobs {
		if job == nil || db.IsTerminalStatus(job.Status) {
			continue
		}
		if job.HasInventoryHost() && !syncTargetFresh(database, job.Host, now) {
			return true
		}
		if job.UsesRentalPlacement() && !syncTargetFresh(database, db.CloudSyncTargetName, now) {
			return true
		}
	}
	return false
}

func syncTargetFresh(database *sql.DB, target string, now time.Time) bool {
	var last time.Time
	if target == db.CloudSyncTargetName {
		last = db.GetLastCloudSync(database)
	} else {
		last = db.GetLastHostSync(database, target)
	}
	if last.IsZero() || last.After(now) {
		return false
	}
	return now.Sub(last) <= daemonFreshSyncMaxAge
}
