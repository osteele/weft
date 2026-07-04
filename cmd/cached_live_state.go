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
	return forceSync
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
