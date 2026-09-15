package dashboard

import (
	"database/sql"
	"time"

	"github.com/osteele/weft/internal/db"
)

// InitialSnapshot is the local DB/cache-backed state used to render the first
// TUI frame before background refreshers have completed.
type InitialSnapshot struct {
	Jobs            []*db.Job
	JobDependencies map[int64]string
	Hosts           []*Host
	HostSyncTimes   map[string]time.Time
}

// LoadInitialSnapshot returns the best-effort startup snapshot using only
// local database and cached host information.
func LoadInitialSnapshot(database *sql.DB, hostCacheDuration time.Duration) InitialSnapshot {
	snapshot, _ := loadInitialSnapshot(database, hostCacheDuration, nil)
	return snapshot
}

// LoadInitialSnapshotForScope returns a startup snapshot whose job query
// applies exact stored attribution before its result limit.
func LoadInitialSnapshotForScope(database *sql.DB, hostCacheDuration time.Duration, scope *JobScope) (InitialSnapshot, error) {
	return loadInitialSnapshot(database, hostCacheDuration, scope)
}

func loadInitialSnapshot(database *sql.DB, hostCacheDuration time.Duration, scope *JobScope) (InitialSnapshot, error) {
	snapshot := InitialSnapshot{
		JobDependencies: make(map[int64]string),
		HostSyncTimes:   make(map[string]time.Time),
	}

	jobs, err := listJobsForScope(database, scope, 1000)
	if err != nil && scope != nil {
		return snapshot, err
	}
	if err == nil {
		snapshot.Jobs = jobs
	}
	deps, err := db.GetJobDependencyInfo(database)
	if err == nil {
		snapshot.JobDependencies = deps
	}

	times, err := db.LoadHostSyncTimes(database)
	if err == nil {
		snapshot.HostSyncTimes = times
	}

	hostNames, err := db.ListHostsForTUI(database)
	if err != nil {
		return snapshot, nil
	}

	cachedHosts, err := db.LoadAllCachedHosts(database)
	if err != nil {
		cachedHosts = nil
	}

	cachedByName := make(map[string]*db.CachedHostInfo, len(cachedHosts))
	for _, cached := range cachedHosts {
		if cached == nil || cached.Name == "" {
			continue
		}
		cachedByName[cached.Name] = cached
	}

	hosts := make([]*Host, 0, len(hostNames))
	for _, name := range hostNames {
		if cached, ok := cachedByName[name]; ok {
			host := hostFromCachedInfo(cached)
			cacheAge := time.Since(time.Unix(cached.LastUpdated, 0))
			if cacheAge > hostCacheDuration {
				host.Status = HostStatusChecking
			} else {
				host.Status = HostStatusOnline
			}
			hosts = append(hosts, host)
			continue
		}
		hosts = append(hosts, &Host{
			Name:   name,
			Status: HostStatusChecking,
		})
	}
	snapshot.Hosts = hosts

	return snapshot, nil
}
