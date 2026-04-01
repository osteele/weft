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
	snapshot := InitialSnapshot{
		JobDependencies: make(map[int64]string),
		HostSyncTimes:   make(map[string]time.Time),
	}

	jobs, err := db.ListJobs(database, "", "", 1000, nil, "")
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

	jobHosts, err := db.ListUniqueHosts(database)
	if err != nil {
		return snapshot
	}

	cachedHosts, err := db.LoadAllCachedHosts(database)
	if err != nil {
		cachedHosts = nil
	}

	cachedByName := make(map[string]*db.CachedHostInfo, len(cachedHosts))
	hostSet := make(map[string]struct{}, len(jobHosts)+len(cachedHosts))
	for _, host := range jobHosts {
		if host == "" {
			continue
		}
		hostSet[host] = struct{}{}
	}
	for _, cached := range cachedHosts {
		if cached == nil || cached.Name == "" {
			continue
		}
		hostSet[cached.Name] = struct{}{}
		cachedByName[cached.Name] = cached
	}

	hostNames := make([]string, 0, len(hostSet))
	for host := range hostSet {
		hostNames = append(hostNames, host)
	}
	naturalSortStrings(hostNames)

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

	return snapshot
}
