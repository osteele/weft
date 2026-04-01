package dashboard

import (
	"github.com/osteele/weft/internal/db"
	"github.com/osteele/weft/internal/hostinfo"
)

func hostFromCachedInfo(cached *db.CachedHostInfo) *Host {
	return hostinfo.HostFromCachedInfo(cached)
}

func cachedInfoFromHost(host *Host) *db.CachedHostInfo {
	return hostinfo.CachedInfoFromHost(host)
}

func updateHostWithCachedStatic(host *Host, cached *Host) {
	hostinfo.UpdateHostWithCachedStatic(host, cached)
}
